package claude

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
)

func TestTheHookGivesClaudeWhatTheDaemonHasForTheTurn(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "daemon.sock")
	tokenFile := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret"), 0o600))
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	defer listener.Close()
	go ipc.Serve(listener, func(request ipc.Request) ipc.Response {
		if request.Op != ipc.OpCheck || request.Token != "secret" {
			return ipc.Response{Error: "wrong request"}
		}
		return ipc.Response{Message: "a new comment arrived"}
	})
	env := map[string]string{SocketVar: socket, TokenFileVar: tokenFile}
	getenv := func(k string) string { return env[k] }

	var out strings.Builder
	require.NoError(t, TurnHook(time.Now(), getenv, strings.NewReader("{}"), &out))
	assert.Contains(t, out.String(), "a new comment arrived")

	out.Reset()
	env[TokenFileVar] = filepath.Join(dir, "missing")
	require.NoError(t, TurnHook(time.Now(), getenv, strings.NewReader("{}"), &out))
	assert.Empty(t, out.String(), "a hook without a token must be quiet")

	out.Reset()
	env[TokenFileVar] = tokenFile
	listener.Close()
	require.NoError(t, TurnHook(time.Now(), getenv, strings.NewReader("{}"), &out))
	assert.Empty(t, out.String(), "a hook without a daemon must be quiet")
}
