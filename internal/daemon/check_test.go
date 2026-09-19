package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

// The turn takes two seconds, so that a mention can arrive while it runs.
const longFakeClaude = `#!/bin/sh
echo "$@" >> "$FAKE_CLAUDE_LOG"
cat > /dev/null
echo '{"type":"system","subtype":"init"}'
sleep 2
echo '{"type":"result","is_error":false,"result":"long answer"}'
`

func (d *Daemon) tokenOfTheTurn(card int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for token, t := range d.turns {
		if t.card == card {
			return token
		}
	}
	return ""
}

func TestAMentionDuringATurnGoesToThatTurn(t *testing.T) {
	fake, server := newTestServer(t)
	script := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(script, []byte(longFakeClaude), 0o755))
	d, _ := startDaemonWith(t, server, script, "")

	fake.mention(7, trustedID)
	var token string
	waitFor(t, "the turn to start", func() bool {
		token = d.tokenOfTheTurn(7)
		return token != ""
	})
	check := func() ipc.Response { return d.handleIPC(ipc.Request{Op: ipc.OpCheck, Token: token}) }
	assert.Empty(t, check().Message, "a check with nothing new must be quiet")

	fake.mention(7, trustedID)
	waitFor(t, "the second mention to be queued", func() bool {
		state, err := d.store.Get(7)
		return err == nil && len(state.Queue) == 2
	})
	response := check()
	assert.Contains(t, response.Message, "during your turn")
	assert.Contains(t, response.Message, "question 2")
	assert.NotContains(t, response.Message, "question 1", "the request that started the turn was shown again")
	assert.Empty(t, check().Message, "a mention was given to the turn two times")

	waitFor(t, "the answer", func() bool { return len(fake.botComments(7)) == 1 })
	time.Sleep(quietTime)
	assert.Len(t, fake.botComments(7), 1, "the mention that the turn got started a turn of its own")
	state, err := d.store.Get(7)
	require.NoError(t, err)
	assert.Empty(t, state.Queue)
}

func TestAMentionDuringATurnOfAnAgentMessageGetsTheFallbackReply(t *testing.T) {
	fake, server := newTestServer(t)
	script := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(script, []byte(longFakeClaude), 0o755))
	d, _ := startDaemonWith(t, server, script, "")

	_, err := d.store.Update(7, func(state *store.CardState) {
		state.Queue = []store.Item{{Kind: store.KindAgentMessage, FromCard: 2, Text: "hello", Hops: 1}}
	})
	require.NoError(t, err)
	d.kick(7)
	var token string
	waitFor(t, "the turn to start", func() bool {
		token = d.tokenOfTheTurn(7)
		return token != ""
	})

	fake.mention(7, trustedID)
	waitFor(t, "the mention to be queued", func() bool {
		state, err := d.store.Get(7)
		return err == nil && len(state.Queue) == 2
	})
	assert.Contains(t, d.handleIPC(ipc.Request{Op: ipc.OpCheck, Token: token}).Message, "question 1")

	waitFor(t, "the fallback reply", func() bool { return len(fake.botComments(7)) == 1 })
	assert.Contains(t, fake.botComments(7)[0], "long answer")
}

func TestAProgressNoteIsAskedForOnceForEachQuietInterval(t *testing.T) {
	_, server := newTestServer(t)
	d, _ := startDaemon(t, server)
	d.cfg.ProgressInterval = config.Duration{Duration: 50 * time.Millisecond}
	token, _, err := d.beginTurn(t.Context(), 1, 0)
	require.NoError(t, err)
	defer d.endTurn(token)
	check := func() ipc.Response { return d.handleIPC(ipc.Request{Op: ipc.OpCheck, Token: token}) }

	assert.Empty(t, check().Message, "a note was asked for before the interval")
	time.Sleep(60 * time.Millisecond)
	assert.Contains(t, check().Message, "progress note")
	assert.Empty(t, check().Message, "a note was asked for two times in one interval")

	time.Sleep(60 * time.Millisecond)
	d.handleIPC(ipc.Request{Op: ipc.OpProgress, Token: token})
	assert.Empty(t, check().Message, "a note was asked for right after a note")
	time.Sleep(60 * time.Millisecond)
	assert.Contains(t, check().Message, "progress note")

	d.cfg.ProgressInterval = config.Duration{}
	time.Sleep(60 * time.Millisecond)
	assert.Empty(t, check().Message, "a note was asked for with the notes turned off")
}
