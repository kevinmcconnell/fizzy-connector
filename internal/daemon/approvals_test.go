package daemon

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
)

func TestParseApprovalAnswer(t *testing.T) {
	cases := map[string]approvalAnswer{
		"yes K7Q2M":               answerYes,
		"@chuck Yes k7q2m.":       answerYes,
		"always K7Q2M":            answerAlways,
		"@chuck no K7Q2M!":        answerNo,
		"yes":                     answerNone,
		"yes K7Q2M but wait":      answerNone,
		"yes, but do not run it":  answerNone,
		"ok K7Q2M":                answerNone,
		"yes K7Q":                 answerNone,
		"what does this command?": answerNone,
		"":                        answerNone,
	}
	for text, want := range cases {
		got, _ := parseApprovalAnswer(text)
		assert.Equal(t, want, got, "%q", text)
	}
}

func TestApprovalSubject(t *testing.T) {
	for _, name := range []string{"--settings={}", "-x", "", "Bash(rm *)", "a b"} {
		_, err := newApprovalSubject(name, json.RawMessage(`{}`))
		assert.Error(t, err, "tool name %q was accepted", name)
	}

	_, err := newApprovalSubject("Bash", json.RawMessage(`{"description":"no command"}`))
	assert.Error(t, err, "a Bash request with no command was accepted")

	large := `{"url":"` + strings.Repeat("x", maxApprovalDisplay) + `"}`
	_, err = newApprovalSubject("WebFetch", json.RawMessage(large))
	assert.Error(t, err, "a request that cannot be shown in full was accepted")

	subject, err := newApprovalSubject("Bash", json.RawMessage("{\"command\":\"echo ```\\n# Heading\"}"))
	require.NoError(t, err)
	assert.Contains(t, subject.question("K7Q2M", time.Minute), "````\necho ```", "the command can close its code block")
}

func (f *fakeFizzy) comment(card int, authorID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments[card] = append(f.comments[card], fizzy.Comment{
		ID:        "a" + time.Now().Format("150405.000000000"),
		CreatedAt: time.Now(),
		Body:      fizzy.Body{HTML: "<p>" + text + "</p>"},
		Creator:   fizzy.User{ID: authorID, Name: authorID},
	})
}

var questionCode = regexp.MustCompile(`yes ([A-Z0-9]{5})`)

func (f *fakeFizzy) lastQuestionCode(t *testing.T, card int) string {
	t.Helper()
	questions := f.botComments(card)
	match := questionCode.FindStringSubmatch(questions[len(questions)-1])
	require.NotNil(t, match, "no code in the question: %s", questions[len(questions)-1])
	return match[1]
}

func assertNoAnswerYet(t *testing.T, responses chan ipc.Response, message string) {
	t.Helper()
	select {
	case response := <-responses:
		require.Fail(t, message, "%+v", response)
	default:
	}
}

func TestApprovalNeedsTheCodeFromATrustedPerson(t *testing.T) {
	fake, server := newTestServer(t)
	d, _ := startDaemon(t, server)

	turnCtx, endTurnContext := context.WithCancel(context.Background())
	defer endTurnContext()
	token, _, err := d.beginTurn(turnCtx, 5, 0)
	require.NoError(t, err)

	ask := func(command string) chan ipc.Response {
		input, _ := json.Marshal(map[string]string{"command": command})
		done := make(chan ipc.Response, 1)
		go func() {
			response, _ := ipc.SendAndWait(d.cfg.SocketPath(), ipc.Request{Op: ipc.OpApprove, Token: token, ToolName: "Bash", ToolInput: input}, 20*time.Second)
			done <- response
		}()
		return done
	}
	botComments := func(count int) func() bool {
		return func() bool { return len(fake.botComments(5)) == count }
	}
	settle := approvalPollInterval + time.Second

	// Two requests at the same time: the card shows one question.
	first, second := ask("go test ./..."), ask("rm -rf build")
	waitFor(t, "first question", botComments(1))
	code := fake.lastQuestionCode(t, 5)

	fake.comment(5, trustedID, "yes")
	fake.comment(5, trustedID, "yes ZZZZZ")
	fake.comment(5, untrustedID, "yes "+code)
	time.Sleep(settle)
	assertNoAnswerYet(t, first, "approved with no valid answer")
	assertNoAnswerYet(t, second, "approved with no valid answer")
	require.Len(t, fake.botComments(5), 1, "the second question must wait for the first")

	// One valid answer approves one request. Then the next question opens.
	fake.comment(5, trustedID, "always "+code)
	var open chan ipc.Response
	select {
	case response := <-first:
		require.True(t, response.Approved, "%+v", response)
		open = second
	case response := <-second:
		require.True(t, response.Approved, "%+v", response)
		open = first
	case <-time.After(waitTimeout):
		require.Fail(t, "no answer")
	}
	waitFor(t, "next question", botComments(2))
	assertNoAnswerYet(t, open, "one answer approved two requests")

	fake.comment(5, trustedID, "no "+fake.lastQuestionCode(t, 5))
	refused := <-open
	assert.False(t, refused.Approved)
	assert.NotEmpty(t, refused.Message)

	// "always" saved the identical command, and only that one.
	state, err := d.store.Get(5)
	require.NoError(t, err)
	require.Len(t, state.AllowedCommands, 1)
	saved := <-ask(state.AllowedCommands[0])
	assert.True(t, saved.Approved, "the saved command was not approved: %+v", saved)
	assert.Len(t, fake.botComments(5), 2, "a saved command must not cause a question")

	// A question ends with its turn. A later answer saves nothing.
	stale := ask("make deploy")
	waitFor(t, "question of the stale request", botComments(3))
	staleCode := fake.lastQuestionCode(t, 5)
	endTurnContext()
	assert.False(t, (<-stale).Approved, "a request was approved after its turn ended")

	fake.comment(5, trustedID, "always "+staleCode)
	time.Sleep(settle)
	state, err = d.store.Get(5)
	require.NoError(t, err)
	assert.Len(t, state.AllowedCommands, 1, "an answer after the turn saved a command")
}

func TestTheBotCannotAnswerItsOwnQuestion(t *testing.T) {
	fake, server := newTestServer(t)
	d, _ := startDaemon(t, server)
	token, _, err := d.beginTurn(context.Background(), 6, 0)
	require.NoError(t, err)
	defer d.endTurn(token)

	d.cfg.TrustedUserIDs = append(d.cfg.TrustedUserIDs, botID)
	done := make(chan ipc.Response, 1)
	go func() {
		response, _ := ipc.SendAndWait(d.cfg.SocketPath(), ipc.Request{Op: ipc.OpApprove, Token: token, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)}, 20*time.Second)
		done <- response
	}()
	waitFor(t, "question", func() bool { return len(fake.botComments(6)) == 1 })

	fake.comment(6, botID, "yes "+fake.lastQuestionCode(t, 6))
	time.Sleep(approvalPollInterval + time.Second)
	assertNoAnswerYet(t, done, "the bot approved its own request")
}

func TestAnOldAnswerCannotAnswerANewQuestion(t *testing.T) {
	fake, server := newTestServer(t)
	d, _ := startDaemon(t, server)
	_, turn, err := d.beginTurn(context.Background(), 8, 0)
	require.NoError(t, err)

	fake.comment(8, trustedID, "always K7Q2M")
	questionID, err := d.client.CreateComment(context.Background(), 8, "<p>question</p>")
	require.NoError(t, err)

	_, answer := d.findApprovalAnswer(turn, questionID, "K7Q2M")
	assert.Equal(t, answerNone, answer, "an answer from before the question was accepted")

	fake.comment(8, trustedID, "yes K7Q2M")
	_, answer = d.findApprovalAnswer(turn, questionID, "K7Q2M")
	assert.Equal(t, answerYes, answer)
}
