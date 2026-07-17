package ui

// Voice dictation for cx.
//
// Flow: ctrl+r starts an ffmpeg process that captures the default mic to a
// 16 kHz mono WAV. A second ctrl+r sends SIGINT to ffmpeg (which finishes
// writing cleanly), POSTs the WAV to Groq's whisper endpoint, and pipes the
// raw transcript through a fast LLM to fix disfluencies, punctuation, and
// custom proper nouns (see ~/.config/cx/dictation-vocab.txt). The cleaned
// text is appended to the current input at the cursor.
//
// Design notes:
//   * ffmpeg over sox: shipped with brew already; sox isn't on the target box.
//   * SIGINT over Kill: ffmpeg's fine with SIGINT — it flushes the WAV
//     container and exits cleanly. Kill would truncate.
//   * Bounded recording: capped at 5 minutes to avoid runaway files if the
//     user forgets to press stop.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"cx/config"
	"cx/llm"
)

const (
	dictationSampleRate = 16000
	dictationMaxMinutes = 5
	groqTranscribeURL   = "https://api.groq.com/openai/v1/audio/transcriptions"
	groqSTTModel        = "whisper-large-v3-turbo"
)

// System sound names (macOS). Fire-and-forget via afplay in a goroutine.
const (
	soundRecStart  = "Tink"   // short crisp ping — begin
	soundRecStop   = "Pop"    // small pop — stopped
	soundTextReady = "Glass"  // soft ding — text landed in input
	soundError     = "Basso"  // low bass — something failed
)

// playSound spawns afplay in a goroutine so it doesn't block the tea loop.
// Silently no-ops if afplay isn't found (Linux users just get no sound; not
// an error). Uses macOS built-in sounds under /System/Library/Sounds.
func playSound(name string) {
	go func() {
		exe, err := exec.LookPath("afplay")
		if err != nil {
			return
		}
		path := fmt.Sprintf("/System/Library/Sounds/%s.aiff", name)
		if _, err := os.Stat(path); err != nil {
			return
		}
		// Detach; caller doesn't wait. afplay exits when the sound ends.
		_ = exec.Command(exe, path).Run()
	}()
}

// dictationSession holds the state of an in-progress recording.
type dictationSession struct {
	cmd     *exec.Cmd
	wavPath string
	started time.Time
	stderr  *bytes.Buffer
}

// active recording, if any. One at a time by construction (the ctrl+r
// handler toggles). Global rather than model-scoped so the tea.Cmd side of
// the pipeline can reach it without threading state.
var currentDictation *dictationSession

// dictationElapsed returns "MM:SS" since the current recording started,
// or "" when nothing's recording.
func dictationElapsed() string {
	if currentDictation == nil {
		return ""
	}
	d := time.Since(currentDictation.started)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// Tea messages so the pipeline can drive the model without blocking the UI.
type (
	dictationStartedMsg struct{ err error }
	dictationDoneMsg    struct {
		text string
		err  error
	}
	// dictationTickMsg fires every ~500ms while recording so the status bar
	// can show elapsed time (see statusView / model.go).
	dictationTickMsg struct{}
)

// dictationTick emits a dictationTickMsg on a ~120ms cadence — fast enough
// for the waveform / spinner animation to feel alive without flooding the
// tea loop. Chained by the model's Update loop while dictating or busy.
func dictationTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return dictationTickMsg{}
	})
}

// startDictationCmd returns a tea.Cmd that spawns ffmpeg and reports the
// outcome. On success, the message carries a nil error and a session is
// registered as currentDictation.
func startDictationCmd(_ *config.Config) tea.Cmd {
	return func() tea.Msg {
		if currentDictation != nil {
			return dictationStartedMsg{err: errors.New("already recording")}
		}
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return dictationStartedMsg{err: errors.New("ffmpeg not found (brew install ffmpeg)")}
		}
		wav := filepath.Join(os.TempDir(), fmt.Sprintf("cx-dictation-%d.wav", os.Getpid()))
		// -f avfoundation is the macOS mic capture backend. ":0" = no video,
		// audio device 0 (the default). If the user has multiple mics and
		// wants a different one, they can list devices with:
		//   ffmpeg -f avfoundation -list_devices true -i ""
		// and change the "0" here later. Format flags force 16 kHz mono PCM
		// so the file is small and Groq's whisper accepts it directly.
		cmd := exec.Command("ffmpeg",
			"-hide_banner", "-loglevel", "error",
			"-f", "avfoundation",
			"-i", ":0",
			"-ac", "1",
			"-ar", fmt.Sprintf("%d", dictationSampleRate),
			"-t", fmt.Sprintf("%d", dictationMaxMinutes*60), // hard cap
			"-y", wav,
		)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return dictationStartedMsg{err: fmt.Errorf("ffmpeg: %w", err)}
		}
		currentDictation = &dictationSession{cmd: cmd, wavPath: wav, started: time.Now(), stderr: &stderr}
		playSound(soundRecStart)
		return dictationStartedMsg{}
	}
}

// stopDictationCmd returns a tea.Cmd that stops the active recording,
// transcribes it, cleans it up, and delivers the final text back to the
// model via dictationDoneMsg.
func stopDictationCmd(cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		sess := currentDictation
		currentDictation = nil
		if sess == nil {
			return dictationDoneMsg{err: errors.New("not recording")}
		}
		playSound(soundRecStop)
		if sess.cmd.Process != nil {
			// SIGINT gives ffmpeg time to flush and close the WAV header.
			// Kill would truncate.
			_ = sess.cmd.Process.Signal(os.Interrupt)
		}
		// Bound the wait: if ffmpeg is stuck for some reason, don't hang
		// the whole UI. 3s is generous — normal shutdown is <500ms.
		done := make(chan error, 1)
		go func() { done <- sess.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = sess.cmd.Process.Kill()
			<-done
		}
		defer os.Remove(sess.wavPath)

		fi, err := os.Stat(sess.wavPath)
		if err != nil {
			return dictationDoneMsg{err: fmt.Errorf("recording missing: %w (ffmpeg stderr: %s)", err, strings.TrimSpace(sess.stderr.String()))}
		}
		if fi.Size() < 4000 {
			// Too short to contain audible speech; likely the user tapped
			// ctrl+r twice by accident. Fail quietly instead of paying for
			// an empty transcription.
			return dictationDoneMsg{err: errors.New("recording too short — hold the mic key longer")}
		}
		// audioBytes is 16 kHz mono S16 PCM = 32,000 bytes/sec. So a 3-second
		// recording is ~96 KB. Below ~2s of net audio we consider it
		// "short" and apply the hallucination filter below.
		wavShort := fi.Size() < 64_000

		key := cfg.Groq.APIKey
		if key == "" {
			return dictationDoneMsg{err: errors.New("groq api key missing — set groq.api_key in config.toml or export GROQ_API_KEY")}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		raw, err := transcribeGroq(ctx, sess.wavPath, key)
		if err != nil {
			return dictationDoneMsg{err: fmt.Errorf("transcribe: %w", err)}
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return dictationDoneMsg{err: errors.New("empty transcript")}
		}
		if wavShort && isWhisperHallucination(raw) {
			// Silence into whisper reliably comes back as "Thank you." /
			// "Thanks for watching." / etc. Drop those on short recordings
			// so you can ctrl+r-twice-by-accident without garbage landing
			// in your prompt.
			return dictationDoneMsg{err: errors.New("nothing heard — silence hallucination filtered")}
		}

		clean, err := cleanupTranscript(ctx, cfg, raw)
		if err != nil || strings.TrimSpace(clean) == "" {
			// Cleanup failure isn't fatal — return the raw transcript so the
			// user still gets their words back. A malformed cleanup that
			// swallows the input is worse than raw text.
			return dictationDoneMsg{text: raw}
		}
		return dictationDoneMsg{text: strings.TrimSpace(clean)}
	}
}

// whisperHallucinations catches the well-known set of phrases the Whisper
// LM emits when it hears silence or non-speech noise (breathing, keyboard,
// fan). They come almost exclusively from YouTube auto-caption training
// data. The list is short by design — we only want to filter obviously-
// hallucinated one-liners on short recordings, not censor real speech.
var whisperHallucinations = []string{
	"thank you",
	"thanks",
	"thanks for watching",
	"thanks for watching!",
	"thank you for watching",
	"thank you for watching!",
	"please subscribe",
	"like and subscribe",
	"see you next time",
	"see you in the next video",
	"bye",
	"bye bye",
	"goodbye",
	"peace",
	"ok",
	"okay",
	"you",
	".",
	"...",
	"[music]",
	"[silence]",
	"(silence)",
}

// isWhisperHallucination reports whether the transcript is one of the
// canonical silence-hallucination phrases. Case- and punctuation-insensitive.
func isWhisperHallucination(raw string) bool {
	norm := strings.ToLower(strings.TrimSpace(raw))
	// Strip trailing punctuation the LM likes to add.
	norm = strings.TrimRight(norm, ".!?,;: ")
	if norm == "" {
		return true
	}
	for _, h := range whisperHallucinations {
		if strings.TrimRight(h, ".!?,;: ") == norm {
			return true
		}
	}
	return false
}

// transcribeGroq POSTs the WAV as multipart/form-data to Groq's OpenAI-
// compatible transcription endpoint. temperature=0 keeps the sampling
// deterministic; the LM head is what hallucinates "Thank you." /
// "Thanks for watching." on silence, and non-zero temp makes it worse.
func transcribeGroq(ctx context.Context, wavPath, apiKey string) (string, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", groqSTTModel)
	_ = mw.WriteField("response_format", "json")
	_ = mw.WriteField("temperature", "0")
	_ = mw.WriteField("language", "en")
	// audio file
	fw, err := mw.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqTranscribeURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("groq HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("groq decode: %w", err)
	}
	if out.Error.Message != "" {
		return "", errors.New(out.Error.Message)
	}
	return out.Text, nil
}

// cleanupTranscript runs the raw transcript through a fast LLM so it comes
// out punctuated, deduped, and with proper-noun corrections applied. The
// prompt is emphatic about NOT paraphrasing — the model has one job and it's
// not to be creative.
func cleanupTranscript(ctx context.Context, cfg *config.Config, raw string) (string, error) {
	model := cfg.DictationModel
	if model == "" {
		model = cfg.MemoryModel
	}
	if model == "" {
		model = "google/gemini-2.5-flash"
	}
	prov, err := llm.ForModel(model, cfg)
	if err != nil {
		return "", err
	}
	vocab := strings.TrimSpace(config.LoadDictationVocab())
	if vocab == "" {
		vocab = "(no custom vocabulary)"
	}
	sys := fmt.Sprintf(`You are a voice-dictation cleanup pass. The user just dictated a message; you receive the raw speech-to-text output and return a cleaned version ready to paste into a chat prompt.

Do:
- Fix punctuation and capitalize sentences.
- Remove pure disfluencies ("um", "uh", meaningless "like"/"you know").
- Split run-on sentences into natural sentences/paragraphs.
- Apply this custom vocabulary — the user has told you these are the correct forms of easily-misheard words:

%s

Do NOT:
- Paraphrase, summarize, add ideas, or invent words.
- Alter meaning. If a word looks wrong but isn't in the vocab, leave it.
- Add commentary, greetings, quotation marks, or explanations of what you did.

Return ONLY the cleaned text. Nothing else.`, vocab)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := prov.Complete(cctx, model, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: raw},
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// ── Animated banner ─────────────────────────────────────────────────────────
//
// The banner floats above the input while dictating or transcribing so the
// state is visible regardless of terminal width — the status-bar indicator
// gets truncated in narrow windows.

// bannerRecStyle draws the recording banner: red accent border, red-ish
// waveform, tight two-line footprint.
var (
	bannerRecStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("196")).
			Foreground(lipgloss.Color("196")).
			Padding(0, 2)

	bannerBusyStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("214")).
			Foreground(lipgloss.Color("214")).
			Padding(0, 2)

	bannerDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// waveformBars renders one row of a moving waveform. The heights come from
// a sum of two sines: a slow "envelope" (the mouth opening/closing) plus a
// faster per-column shimmer, so it looks like real audio, not a metronome.
// Bar rune ladder is 1..8 stops of Unicode block-eighths.
func waveformBars(frame, width int) string {
	if width < 4 {
		return ""
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	var sb strings.Builder
	for i := 0; i < width; i++ {
		envelope := math.Sin(float64(frame)*0.20 + float64(i)*0.15)
		shimmer := math.Sin(float64(frame)*0.55 + float64(i)*1.31)
		combined := (envelope*0.7 + shimmer*0.3 + 1) / 2 // 0..1
		if combined < 0 {
			combined = 0
		}
		if combined > 1 {
			combined = 1
		}
		idx := int(combined * float64(len(bars)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bars) {
			idx = len(bars) - 1
		}
		sb.WriteRune(bars[idx])
	}
	return sb.String()
}

// brailleSpinner returns one frame of the classic 8-phase braille spinner.
func brailleSpinner(frame int) rune {
	frames := []rune("⣾⣽⣻⢿⡿⣟⣯⣷")
	return frames[((frame%len(frames))+len(frames))%len(frames)]
}

// transcribeDots animates "TRANSCRIBING…" by cycling the dot count.
func transcribeDots(frame int) string {
	n := (frame / 3) % 4 // 0..3
	return strings.Repeat(".", n) + strings.Repeat(" ", 3-n)
}

// dictationBannerView renders the two-row animated overlay above the prompt.
// Returns "" when neither dictating nor busy.
func (m Model) dictationBannerView() string {
	if !m.dictating && !m.dictationBusy {
		return ""
	}
	inner := m.width - 8 // border (2) + padding (2*2) + tiny slack
	if inner < 20 {
		inner = 20
	}
	if m.dictating {
		elapsed := dictationElapsed()
		if elapsed == "" {
			elapsed = "0:00"
		}
		spin := string(brailleSpinner(m.dictationFrame))
		label := fmt.Sprintf("%s  REC  %s", spin, elapsed)
		hint := "ctrl+r to stop"
		labelStyled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196")).Render(label)
		hintStyled := bannerDimStyle.Render(hint)
		// Compose header row: label on left, hint on right, pad in between
		headerW := lipgloss.Width(labelStyled) + lipgloss.Width(hintStyled)
		pad := inner - headerW
		if pad < 1 {
			pad = 1
		}
		header := labelStyled + strings.Repeat(" ", pad) + hintStyled
		wave := waveformBars(m.dictationFrame, inner)
		body := header + "\n" + wave
		return bannerRecStyle.Width(inner + 4).Render(body)
	}
	// Busy / transcribing
	spin := string(brailleSpinner(m.dictationFrame))
	label := fmt.Sprintf("%s  TRANSCRIBING%s", spin, transcribeDots(m.dictationFrame))
	hint := "cleaning up + custom vocab…"
	labelStyled := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render(label)
	hintStyled := bannerDimStyle.Render(hint)
	headerW := lipgloss.Width(labelStyled) + lipgloss.Width(hintStyled)
	pad := inner - headerW
	if pad < 1 {
		pad = 1
	}
	header := labelStyled + strings.Repeat(" ", pad) + hintStyled
	// A subtle scrolling underline of dots so the pipeline still feels alive.
	underline := strings.Repeat("·", inner)
	// Rotate a "highlight window" across the underline.
	hlWidth := 8
	hlStart := m.dictationFrame % (inner + hlWidth)
	if hlStart >= inner {
		hlStart = 0
	}
	hlEnd := hlStart + hlWidth
	if hlEnd > inner {
		hlEnd = inner
	}
	prefix := underline[:hlStart]
	middle := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render(underline[hlStart:hlEnd])
	suffix := bannerDimStyle.Render(underline[hlEnd:])
	prefixStyled := bannerDimStyle.Render(prefix)
	body := header + "\n" + prefixStyled + middle + suffix
	return bannerBusyStyle.Width(inner + 4).Render(body)
}
