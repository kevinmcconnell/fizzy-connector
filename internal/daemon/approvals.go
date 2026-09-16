package daemon

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

const (
	approvalPollInterval = 2 * time.Second
	maxApprovalDisplay   = 4000
	codeAlphabet         = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	codeLength           = 5
	maxPurposeLength     = 500
)

type approvalAnswer int

const (
	answerNone approvalAnswer = iota
	answerYes
	answerAlways
	answerNo
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)

// parseApprovalAnswer accepts only "<yes|always|no> <code>", after the
// mentions. A sentence such as "yes, but wait" is not an answer.
func parseApprovalAnswer(plainText string) (approvalAnswer, string) {
	var words []string
	for word := range strings.FieldsSeq(plainText) {
		if !strings.HasPrefix(word, "@") || len(words) > 0 {
			words = append(words, word)
		}
	}
	if len(words) != 2 {
		return answerNone, ""
	}

	code := strings.ToUpper(strings.TrimRight(words[1], ".!"))
	if len(code) != codeLength || strings.Trim(code, codeAlphabet) != "" {
		return answerNone, ""
	}
	switch strings.ToLower(words[0]) {
	case "yes":
		return answerYes, code
	case "always":
		return answerAlways, code
	case "no":
		return answerNo, code
	}
	return answerNone, ""
}

func isApprovalAnswer(plainText string) bool {
	answer, _ := parseApprovalAnswer(plainText)
	return answer != answerNone
}

func newApprovalCode() string {
	raw := make([]byte, codeLength)
	rand.Read(raw)
	for i := range raw {
		raw[i] = codeAlphabet[int(raw[i])%len(codeAlphabet)]
	}
	return string(raw)
}

// fenced puts text in a Markdown code block that the text cannot close: the
// fence is longer than the longest run of backticks in the text.
func fenced(text string) string {
	longest, run := 0, 0
	for _, char := range text {
		if char == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + text + "\n" + fence
}

type approvalSubject struct {
	toolName string
	command  string
	display  string
	purpose  string
}

func (s approvalSubject) isCommand() bool { return s.toolName == "Bash" }

func newApprovalSubject(toolName string, input json.RawMessage) (approvalSubject, error) {
	subject := approvalSubject{toolName: toolName}
	if !toolNamePattern.MatchString(toolName) {
		return subject, fmt.Errorf("%q is not a tool name", toolName)
	}

	if subject.isCommand() {
		var fields struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		if json.Unmarshal(input, &fields) != nil || strings.TrimSpace(fields.Command) == "" {
			return subject, fmt.Errorf("the Bash request has no command")
		}
		subject.command, subject.display, subject.purpose = fields.Command, fields.Command, fields.Description
		if purpose := []rune(subject.purpose); len(purpose) > maxPurposeLength {
			subject.purpose = string(purpose[:maxPurposeLength]) + " ..."
		}
	} else {
		var pretty bytes.Buffer
		if json.Indent(&pretty, input, "", "  ") != nil {
			return subject, fmt.Errorf("the tool input is not JSON")
		}
		subject.display = pretty.String()
	}

	if len(subject.display) > maxApprovalDisplay {
		return subject, fmt.Errorf("the request is too large to show to a person in full (%d characters)", len(subject.display))
	}
	return subject, nil
}

func (s approvalSubject) question(code string, wait time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔐 **Permission needed** (request `%s`)\n\nI want to use the tool `%s` with this input:\n\n%s\n\n", code, s.toolName, fenced(s.display))
	if s.purpose != "" {
		fmt.Fprintf(&b, "The purpose that I state (not verified):\n\n%s\n\n", fenced(s.purpose))
	}
	fmt.Fprintf(&b, "To answer, comment with exactly one of these:\n\n- `yes %s` permits it one time\n", code)
	if s.isCommand() {
		fmt.Fprintf(&b, "- `always %s` also permits this identical command in later turns of this card\n", code)
	}
	fmt.Fprintf(&b, "- `no %s` refuses it\n\nI wait for %s.", code, wait)
	return b.String()
}

func (d *Daemon) approvalPending(number int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.approvals[number] > 0
}

func (d *Daemon) setApprovalPending(number int, delta int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.approvals[number] += delta
}

// approvalGate lets one question at a time be open on a card.
func (d *Daemon) approvalGate(number int) chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.approvalGates[number] == nil {
		d.approvalGates[number] = make(chan struct{}, 1)
	}
	return d.approvalGates[number]
}

// requestApproval asks the trusted people on the card of the turn, and waits
// for an answer to this question. One time limit covers the wait for the
// gate and the wait for the answer, and all of it ends with the turn.
func (d *Daemon) requestApproval(t *turn, request ipc.Request) ipc.Response {
	subject, err := newApprovalSubject(request.ToolName, request.ToolInput)
	if err != nil {
		d.logger.Warn("permission request refused", "card", t.card, "error", err)
		return ipc.Response{Message: "The connector refused this permission request: " + err.Error() + ". Do not try it again."}
	}

	wait := d.cfg.ApprovalTimeout.Duration
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	turnEnded := ipc.Response{Message: "The turn ended before a person answered."}
	timedOut := ipc.Response{Message: fmt.Sprintf("No trusted person answered within %s. Do not try this action again. Say in your reply what you could not do.", wait)}

	d.setApprovalPending(t.card, 1)
	defer d.setApprovalPending(t.card, -1)

	gate := d.approvalGate(t.card)
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-t.ctx.Done():
		return turnEnded
	case <-deadline.C:
		return timedOut
	}

	if subject.isCommand() {
		state, err := d.store.Get(t.card)
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		for _, allowed := range state.AllowedCommands {
			if allowed == subject.command && t.ctx.Err() == nil {
				return ipc.Response{Approved: true}
			}
		}
	}

	code := newApprovalCode()
	questionID, err := d.client.CreateComment(t.ctx, t.card, fizzy.MarkdownToHTML(subject.question(code, wait)))
	if err != nil {
		return ipc.Response{Error: err.Error()}
	}
	d.logger.Info("permission requested", "card", t.card, "request", code, "tool", subject.toolName, "input", subject.display)

	for {
		select {
		case <-t.ctx.Done():
			return turnEnded
		case <-deadline.C:
			d.logger.Info("permission request timed out", "card", t.card, "request", code)
			return timedOut
		case <-time.After(approvalPollInterval):
		}

		comment, answer := d.findApprovalAnswer(t, questionID, code)
		if answer == answerNone {
			continue
		}

		// The answer counts only while the turn is alive, and one time.
		granted := false
		_, err := d.store.Update(t.card, func(state *store.CardState) {
			if t.ctx.Err() != nil || state.IsHandled(comment.ID) {
				return
			}
			granted = true
			state.Handled = append(state.Handled, comment.ID)
			if answer == answerAlways && subject.isCommand() {
				state.AllowedCommands = append(state.AllowedCommands, subject.command)
			}
		})
		if err != nil {
			return ipc.Response{Error: err.Error()}
		}
		if !granted {
			continue
		}
		d.logger.Info("permission answered", "card", t.card, "request", code, "by", comment.Creator.Name, "answer", answer)
		if answer == answerNo {
			return ipc.Response{Message: comment.Creator.Name + " refused this action in Fizzy. Do not try it again."}
		}
		return ipc.Response{Approved: true}
	}
}

// findApprovalAnswer looks only at the comments after the question, so that
// an old answer with the same code cannot answer a new question. An answer
// must be a new comment: a comment that was edited into an answer counts too,
// if it is after the question.
func (d *Daemon) findApprovalAnswer(t *turn, questionID, code string) (fizzy.Comment, approvalAnswer) {
	comments, err := d.client.Comments(t.ctx, t.card)
	if err != nil {
		return fizzy.Comment{}, answerNone
	}

	afterQuestion := false
	for _, comment := range comments {
		if comment.ID == questionID {
			afterQuestion = true
			continue
		}
		author := comment.Creator.ID
		if !afterQuestion || author == d.cfg.BotUserID || !d.cfg.IsTrusted(author) {
			continue
		}
		if answer, answerCode := parseApprovalAnswer(fizzy.PlainText(comment.Body.HTML)); answerCode == code {
			return comment, answer
		}
	}
	return fizzy.Comment{}, answerNone
}

func (a approvalAnswer) String() string {
	return [...]string{"none", "yes", "always", "no"}[a]
}
