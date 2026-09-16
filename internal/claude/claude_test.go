package claude

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fake claude starts a child that keeps stdout open, as a shell command of
// a real session can do.
const fakeClaude = `#!/bin/sh
sleep 300 &
echo $! > "$CHILD_PID_FILE"
echo '{"type":"system","subtype":"init"}'
wait
`

func TestStopEndsTheProcessGroup(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(script, []byte(fakeClaude), 0o755))
	pidFile := filepath.Join(dir, "child.pid")
	t.Setenv("CHILD_PID_FILE", pidFile)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	started := time.Now()
	result, err := Turn{ClaudePath: script, Dir: dir, SessionID: "s", Log: io.Discard}.Run(ctx)

	require.Error(t, err)
	assert.True(t, result.SessionStarted)
	assert.Less(t, time.Since(started), 8*time.Second, "a child process kept the turn open")

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 2*time.Second, 50*time.Millisecond,
		"the child process %d is still alive", pid)
}

func TestStartFailureIsReported(t *testing.T) {
	_, err := Turn{ClaudePath: "/does/not/exist", Dir: t.TempDir(), Log: io.Discard}.Run(context.Background())

	assert.ErrorIs(t, err, ErrNotStarted)
}
