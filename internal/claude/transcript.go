package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The turn sets this variable for the claude process. The hook writes the
// path of the transcript of the session to this file, so the turn can read
// the exact usage of its calls after the process ended. The stream has the
// output tokens of a call only in the result event, which a stopped turn
// does not have.
const TranscriptFileVar = "FIZZY_TURN_TRANSCRIPT_FILE"

type transcriptLine struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage tokens `json:"usage"`
	} `json:"message"`
}

// readTranscript returns the calls since a time, from the transcript of the
// session and from the transcripts of its subagents.
func readTranscript(path string, since time.Time) ([]call, error) {
	calls, err := readCalls(path, since, false)
	if err != nil {
		return nil, err
	}
	session := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	subagents, _ := filepath.Glob(filepath.Join(filepath.Dir(path), session, "subagents", "*.jsonl"))
	for _, subagent := range subagents {
		more, err := readCalls(subagent, since, true)
		if err != nil {
			return nil, err
		}
		calls = append(calls, more...)
	}
	return calls, nil
}

func readCalls(path string, since time.Time, subagent bool) ([]call, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var calls []call
	index := map[string]int{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		var line transcriptLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Type != "assistant" || line.Message.ID == "" || line.Timestamp.Before(since) {
			continue
		}
		c := call{model: line.Message.Model, subagent: subagent, tokens: line.Message.Usage}
		if i, seen := index[line.Message.ID]; seen {
			calls[i] = c
		} else {
			index[line.Message.ID] = len(calls)
			calls = append(calls, c)
		}
	}
	return calls, scanner.Err()
}

// transcriptCalls reads the calls of the turn when the hook recorded the
// transcript. It returns nil when it did not.
func (t Turn) transcriptCalls(since time.Time) []call {
	if t.TranscriptFile == "" {
		return nil
	}
	path, err := os.ReadFile(t.TranscriptFile)
	os.Remove(t.TranscriptFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return nil
	}
	calls, err := readTranscript(strings.TrimSpace(string(path)), since)
	if err != nil {
		return nil
	}
	return calls
}
