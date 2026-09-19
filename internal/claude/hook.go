package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
)

// The turn sets these variables for the claude process, and the hook reads
// them.
const (
	DeadlineVar  = "FIZZY_TURN_DEADLINE"
	WarnAtVar    = "FIZZY_TURN_WARN_AT"
	SocketVar    = "FIZZY_TURN_SOCKET"
	TokenFileVar = "FIZZY_TURN_TOKEN_FILE"
)

// The hook must answer within the timeout of its settings, also when the
// daemon reads the card to answer.
const checkWait = 8 * time.Second

// TurnHook is the PostToolUse hook of a turn. It records the transcript
// path of the session, and after each tool call it gives Claude what the
// daemon has for the turn: new comments on the card, and a request for a
// progress note. From the warning time on, it also tells Claude when the
// turn ends, so that Claude commits the finished work and replies before
// the stop.
func TurnHook(now time.Time, getenv func(string) string, in io.Reader, out io.Writer) error {
	var input struct {
		TranscriptPath string `json:"transcript_path"`
	}
	json.NewDecoder(in).Decode(&input)
	if file := getenv(TranscriptFileVar); file != "" && input.TranscriptPath != "" {
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			os.WriteFile(file, []byte(input.TranscriptPath), 0o600)
		}
	}

	var notes []string
	if note := checkWithDaemon(getenv); note != "" {
		notes = append(notes, note)
	}
	if note := deadlineNote(now, getenv); note != "" {
		notes = append(notes, note)
	}
	if len(notes) == 0 {
		return nil
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": strings.Join(notes, "\n\n")},
	})
}

// checkWithDaemon is quiet when the session has no turn in the daemon, or
// when the daemon does not answer: the work goes on without the note.
func checkWithDaemon(getenv func(string) string) string {
	socket, tokenFile := getenv(SocketVar), getenv(TokenFileVar)
	if socket == "" || tokenFile == "" {
		return ""
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return ""
	}
	response, err := ipc.SendAndWait(socket, ipc.Request{Op: ipc.OpCheck, Token: string(token)}, checkWait)
	if err != nil {
		return ""
	}
	return response.Message
}

func deadlineNote(now time.Time, getenv func(string) string) string {
	deadline, err := time.Parse(time.RFC3339, getenv(DeadlineVar))
	if err != nil {
		return ""
	}
	warnAt, err := time.Parse(time.RFC3339, getenv(WarnAtVar))
	if err != nil || now.Before(warnAt) {
		return ""
	}
	left := deadline.Sub(now).Round(time.Minute)
	if left < time.Minute {
		return fmt.Sprintf("This turn is stopped at %s, in less than a minute. Post your reply now.", deadline.Local().Format("15:04"))
	}
	return fmt.Sprintf("This turn is stopped at %s, in about %s. Nothing that you do after that is saved. "+
		"Commit the work that is finished now, and post your reply now, even if the work is incomplete.",
		deadline.Local().Format("15:04"), left)
}
