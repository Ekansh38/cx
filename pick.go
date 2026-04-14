package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"cx/store"
)

// pickConversation shows an fzf picker over existing conversations.
// Returns the selected conversation's ID (creating a new one if needed).
// Falls back to most recent conversation if fzf is not installed.
func pickConversation(s *store.Store, mdl string) (int64, error) {
	convs, err := s.ListConversations()
	if err != nil {
		return 0, err
	}

	if len(convs) == 0 {
		c, err := s.CreateConversation(mdl)
		if err != nil {
			return 0, err
		}
		return c.ID, nil
	}

	// if fzf not available, just resume most recent
	if _, err := exec.LookPath("fzf"); err != nil {
		return convs[0].ID, nil
	}

	return fzfPick(s, convs, mdl)
}

func fzfPick(s *store.Store, convs []*store.Conversation, mdl string) (int64, error) {
	var sb strings.Builder

	// "new" option at top -- ID field is 0
	sb.WriteString("0\t+ new conversation\t\t\n")

	for _, c := range convs {
		date := time.Unix(c.UpdatedAt, 0).Format("Jan 02")
		msgs := ""
		if c.MessageCount > 0 {
			msgs = fmt.Sprintf("%d msgs", c.MessageCount)
		}
		sb.WriteString(fmt.Sprintf("%d\t%s\t%s\t%s\n", c.ID, c.Title, date, msgs))
	}

	cmd := exec.Command("fzf",
		"--delimiter=\t",
		"--with-nth=2..",       // hide raw ID column
		"--height=50%",
		"--reverse",
		"--prompt=> ",
		"--header=  cx conversations",
		"--no-info",
	)
	cmd.Stdin = strings.NewReader(sb.String())

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		// user hit Esc or fzf failed -- resume most recent
		return convs[0].ID, nil
	}

	line := strings.TrimSpace(out.String())
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) == 0 {
		return convs[0].ID, nil
	}

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return convs[0].ID, nil
	}

	if id == 0 {
		c, err := s.CreateConversation(mdl)
		if err != nil {
			return 0, err
		}
		return c.ID, nil
	}

	return id, nil
}
