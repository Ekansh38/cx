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
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"cx/config"
	"cx/llm"
)

const (
	dictationSampleRate = 16000
	dictationMaxMinutes = 5
	groqTranscribeURL   = "https://api.groq.com/openai/v1/audio/transcriptions"
	groqSTTModel        = "whisper-large-v3-turbo"
)

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

// Tea messages so the pipeline can drive the model without blocking the UI.
type (
	dictationStartedMsg struct{ err error }
	dictationDoneMsg    struct {
		text string
		err  error
	}
)

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

// transcribeGroq POSTs the WAV as multipart/form-data to Groq's OpenAI-
// compatible transcription endpoint.
func transcribeGroq(ctx context.Context, wavPath, apiKey string) (string, error) {
	f, err := os.Open(wavPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// model + response format
	_ = mw.WriteField("model", groqSTTModel)
	_ = mw.WriteField("response_format", "json")
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
