package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
)

const (
	botID       = "bot1"
	trustedID   = "kevin1"
	untrustedID = "guest1"
)

const fakeClaude = `#!/bin/sh
echo "$@" >> "$FAKE_CLAUDE_LOG"
card=$(echo "$@" | sed -n 's/.*"--card","\([0-9]*\)".*/\1/p')
mode=new
case "$*" in *--resume*) mode=resume;; esac
cat > /dev/null
sleep 0.3
echo '{"type":"system","subtype":"init"}'
echo "{\"type\":\"result\",\"is_error\":false,\"result\":\"answer card $card $mode\"}"
`

type fakeFizzy struct {
	mu            sync.Mutex
	comments      map[int][]fizzy.Comment
	descriptions  map[int]string
	notifications []fizzy.Notification
	reactions     []string
	members       []string
	nextID        int
}

func mentionHTML(text string) string {
	envelope := fmt.Sprintf(`{"_rails":{"data":"gid://fizzy/User/%s","pur":"attachable"}}`, botID)
	sgid := base64.StdEncoding.EncodeToString([]byte(envelope)) + "--sig"
	return fmt.Sprintf(`<p><action-text-attachment sgid="%s" content-type="application/vnd.actiontext.mention">Claude</action-text-attachment> %s</p>`, sgid, text)
}

func (f *fakeFizzy) mention(card int, authorID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	html := mentionHTML(fmt.Sprintf("question %d", f.nextID))
	f.comments[card] = append(f.comments[card], fizzy.Comment{
		ID:        "c" + strconv.Itoa(f.nextID),
		CreatedAt: time.Now(),
		Body:      fizzy.Body{HTML: html},
		Creator:   fizzy.User{ID: authorID, Name: authorID},
	})
	f.notify(card, "mention", authorID, fizzy.PlainText(html))
}

// notify does what Fizzy does: a card has one notification for each user, and
// a new event replaces its source and makes it unread again.
func (f *fakeFizzy) notify(card int, sourceType, creatorID, body string) {
	replacement := fizzy.Notification{
		ID:         "n" + strconv.Itoa(card),
		SourceType: sourceType,
		Body:       body,
		Creator:    fizzy.User{ID: creatorID, Name: creatorID},
		Card:       fizzy.NotificationCard{Number: card},
	}
	for i := range f.notifications {
		if f.notifications[i].Card.Number == card {
			f.notifications[i] = replacement
			return
		}
	}
	f.notifications = append(f.notifications, replacement)
}

func (f *fakeFizzy) event(card int, creatorID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notify(card, "event", creatorID, "moved the card")
}

func (f *fakeFizzy) describe(card int, mentionerID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.descriptions[card] = mentionHTML(text)
	f.notify(card, "mention", mentionerID, fizzy.PlainText(f.descriptions[card]))
}

func (f *fakeFizzy) botComments(card int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bodies []string
	for _, comment := range f.comments[card] {
		if comment.Creator.ID == botID {
			bodies = append(bodies, comment.Body.HTML)
		}
	}
	return bodies
}

func (f *fakeFizzy) unreadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, notification := range f.notifications {
		if !notification.Read {
			count++
		}
	}
	return count
}

func (f *fakeFizzy) setMembers(userIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = userIDs
}

var (
	accessesPath = regexp.MustCompile(`^/1/boards/(\w+)/accesses$`)
	cardPath     = regexp.MustCompile(`^/1/cards/(\d+)$`)
	cardReaction = regexp.MustCompile(`^/1/cards/(\d+)/reactions$`)
	commentsPath = regexp.MustCompile(`^/1/cards/(\d+)/comments$`)
	reactionPath = regexp.MustCompile(`^/1/cards/(\d+)/comments/(\w+)/reactions$`)
	readingPath  = regexp.MustCompile(`^/1/notifications/(\w+)/reading$`)
)

func (f *fakeFizzy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	respond := func(value any) { json.NewEncoder(w).Encode(value) }
	path := r.URL.Path

	switch {
	case path == "/my/identity":
		respond(fizzy.Identity{Accounts: []fizzy.Account{{Slug: "/1", User: fizzy.User{ID: botID, Name: "Claude"}}}})
	case path == "/1/notifications":
		respond(f.notifications)
	case readingPath.MatchString(path):
		id := readingPath.FindStringSubmatch(path)[1]
		for i := range f.notifications {
			if f.notifications[i].ID == id {
				f.notifications[i].Read = true
			}
		}
	case cardPath.MatchString(path):
		number, _ := strconv.Atoi(cardPath.FindStringSubmatch(path)[1])
		if number == 404 {
			http.NotFound(w, r)
			return
		}
		respond(fizzy.Card{
			Number: number, Title: "Card " + strconv.Itoa(number), Creator: fizzy.User{ID: trustedID},
			Description: fizzy.PlainText(f.descriptions[number]), DescriptionHTML: f.descriptions[number],
			Board: fizzy.Board{ID: "b1", Name: "Board"},
		})
	case accessesPath.MatchString(path):
		f.serveAccesses(w, r)
	case commentsPath.MatchString(path):
		number, _ := strconv.Atoi(commentsPath.FindStringSubmatch(path)[1])
		if r.Method == http.MethodGet {
			respond(append([]fizzy.Comment{}, f.comments[number]...))
			return
		}
		var payload struct {
			Comment struct{ Body string } `json:"comment"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		f.nextID++
		created := fizzy.Comment{
			ID:        "c" + strconv.Itoa(f.nextID),
			CreatedAt: time.Now(),
			Body:      fizzy.Body{HTML: payload.Comment.Body},
			Creator:   fizzy.User{ID: botID, Name: "Claude"},
		}
		f.comments[number] = append(f.comments[number], created)
		w.Header().Set("Location", path+"/"+created.ID+".json")
		w.WriteHeader(http.StatusCreated)
		respond(created)
	case cardReaction.MatchString(path):
		f.reactions = append(f.reactions, "card")
		w.WriteHeader(http.StatusCreated)
	case reactionPath.MatchString(path):
		f.reactions = append(f.reactions, reactionPath.FindStringSubmatch(path)[2])
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

// serveAccesses lists all users of the account, one user for each page, like
// Fizzy lists them with a Link header for the next page.
func (f *fakeFizzy) serveAccesses(w http.ResponseWriter, r *http.Request) {
	users := []string{botID, trustedID, untrustedID}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	if page < len(users) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
	}
	user := users[page-1]
	access := fizzy.BoardAccess{HasAccess: slices.Contains(f.members, user)}
	access.ID, access.Name = user, user
	json.NewEncoder(w).Encode(map[string]any{
		"board_id":   "b1",
		"all_access": false,
		"users":      []fizzy.BoardAccess{access},
	})
}

func newFakeFizzy() *fakeFizzy {
	return &fakeFizzy{
		comments:     map[int][]fizzy.Comment{},
		descriptions: map[int]string{},
		members:      []string{botID, trustedID, untrustedID},
	}
}

const (
	waitTimeout = 10 * time.Second
	waitTick    = 20 * time.Millisecond
	quietTime   = 600 * time.Millisecond
)

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, waitTimeout, waitTick, what)
}

func newTestServer(t *testing.T) (*fakeFizzy, *httptest.Server) {
	t.Helper()
	fake := newFakeFizzy()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func startDaemon(t *testing.T, server *httptest.Server) (*Daemon, string) {
	t.Helper()
	return startDaemonWith(t, server, "", "")
}

func startDaemonWith(t *testing.T, server *httptest.Server, claudePath, mcpCommand string) (*Daemon, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	if claudePath == "" {
		claudePath = filepath.Join(dir, "claude")
		require.NoError(t, os.WriteFile(claudePath, []byte(fakeClaude), 0o755))
	}
	claudeLog := filepath.Join(dir, "claude.log")
	t.Setenv("FAKE_CLAUDE_LOG", claudeLog)

	cfg := config.Defaults()
	cfg.BaseURL, cfg.AccountSlug, cfg.Token = server.URL, "1", "token"
	cfg.BotUserID, cfg.TrustedUserIDs = botID, []string{trustedID}
	cfg.Repo, cfg.StateDir, cfg.ClaudePath = dir, filepath.Join(dir, "state"), claudePath
	cfg.PollInterval = config.Duration{Duration: 100 * time.Millisecond}
	cfg.MaxConcurrent = 2

	cfg.Path = filepath.Join(dir, "config.toml")
	require.NoError(t, cfg.Write(cfg.Path))

	d, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	if mcpCommand != "" {
		d.mcpCommand = mcpCommand
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done, "daemon")
	})
	waitFor(t, "daemon socket", func() bool {
		_, err := os.Stat(cfg.SocketPath())
		return err == nil
	})
	return d, claudeLog
}

func TestRepliesGoToTheCardOfTheMention(t *testing.T) {
	fake, server := newTestServer(t)
	fake.mention(1, trustedID)
	fake.mention(2, trustedID)
	fake.mention(3, untrustedID)
	_, claudeLog := startDaemon(t, server)

	waitFor(t, "answers on cards 1 and 2", func() bool {
		return len(fake.botComments(1)) == 1 && len(fake.botComments(2)) == 1
	})
	assert.Contains(t, fake.botComments(1)[0], "answer card 1 new")
	assert.Contains(t, fake.botComments(2)[0], "answer card 2 new")
	waitFor(t, "all notifications read", func() bool { return fake.unreadCount() == 0 })
	assert.Empty(t, fake.botComments(3), "a mention from a person who is not trusted got an answer")

	fake.mention(1, trustedID)
	waitFor(t, "second answer on card 1", func() bool { return len(fake.botComments(1)) == 2 })
	assert.Contains(t, fake.botComments(1)[1], "answer card 1 resume")
	assert.Len(t, fake.botComments(2), 1)

	log, err := os.ReadFile(claudeLog)
	require.NoError(t, err)
	started := regexp.MustCompile(`--session-id (\S+) .*"--card","1"`).FindSubmatch(log)
	resumed := regexp.MustCompile(`--resume (\S+) .*"--card","1"`).FindSubmatch(log)
	require.NotNil(t, started, "%s", log)
	require.NotNil(t, resumed, "%s", log)
	assert.Equal(t, string(started[1]), string(resumed[1]), "card 1 did not resume its own session")
	assert.Len(t, fake.reactions, 3, "acknowledge reactions")
}

func TestAMentionIsNotLostBehindALaterEvent(t *testing.T) {
	fake, server := newTestServer(t)
	fake.mention(1, trustedID)
	fake.event(1, untrustedID)
	startDaemon(t, server)

	waitFor(t, "answer on card 1", func() bool { return len(fake.botComments(1)) == 1 })
}

func TestANotificationForADeletedCardIsDropped(t *testing.T) {
	fake, server := newTestServer(t)
	fake.mention(404, trustedID)
	fake.mention(1, trustedID)
	startDaemon(t, server)

	waitFor(t, "answer on card 1", func() bool { return len(fake.botComments(1)) == 1 })
	waitFor(t, "all notifications read", func() bool { return fake.unreadCount() == 0 })
}

func TestTrimLogKeepsTheNewestLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "card.log")
	var content strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&content, "line %04d\n", i)
	}
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o600))

	require.NoError(t, trimLog(path, 2000))

	trimmed, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(trimmed), 1000)
	assert.True(t, strings.HasPrefix(string(trimmed), "line 0"), "the log must start at a line start")
	assert.True(t, strings.HasSuffix(string(trimmed), "line 0999\n"), "the log must keep the newest line")
}

func TestRemoveOldLogsKeepsTheRecentOnes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, age time.Duration) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("log\n"), 0o600))
		require.NoError(t, os.Chtimes(path, now.Add(-age), now.Add(-age)))
	}
	write("card-1.log", 31*24*time.Hour)
	write("card-2.log", 29*24*time.Hour)
	write("card-3.log.tmp", 31*24*time.Hour)
	write("card-4.log", 31*24*time.Hour)
	write("notes.txt", 31*24*time.Hour)
	remove := func(card int, path string) (bool, error) {
		if card == 4 {
			return false, nil
		}
		return true, os.Remove(path)
	}

	removed, err := removeOldLogs(dir, 30*24*time.Hour, now, remove)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	assert.NoFileExists(t, filepath.Join(dir, "card-1.log"))
	assert.FileExists(t, filepath.Join(dir, "card-2.log"))
	assert.FileExists(t, filepath.Join(dir, "card-3.log.tmp"))
	assert.FileExists(t, filepath.Join(dir, "card-4.log"), "the log of a running turn stays")
	assert.FileExists(t, filepath.Join(dir, "notes.txt"))

	removed, err = removeOldLogs(filepath.Join(dir, "missing"), time.Hour, now, remove)
	require.NoError(t, err)
	assert.Equal(t, 0, removed)
}

func TestADescriptionMentionNeedsATrustedMentioner(t *testing.T) {
	fake, server := newTestServer(t)
	fake.describe(1, untrustedID, "delete all files")
	fake.describe(2, trustedID, "what is the plan?")
	startDaemon(t, server)

	waitFor(t, "answer on card 2", func() bool { return len(fake.botComments(2)) == 1 })
	waitFor(t, "all notifications read", func() bool { return fake.unreadCount() == 0 })

	fake.mention(1, trustedID)
	waitFor(t, "answer to the comment on card 1", func() bool { return len(fake.botComments(1)) == 1 })
	time.Sleep(quietTime)
	assert.Len(t, fake.botComments(1), 1, "the description of a person who is not trusted started a turn")
}

func TestALongDescriptionGetsANoticeAndNoTurn(t *testing.T) {
	fake, server := newTestServer(t)
	fake.describe(1, trustedID, strings.Repeat("a long description ", 20))
	_, claudeLog := startDaemon(t, server)

	waitFor(t, "notice on card 1", func() bool { return len(fake.botComments(1)) == 1 })
	assert.Contains(t, fake.botComments(1)[0], "longer than 200 characters")
	waitFor(t, "all notifications read", func() bool { return fake.unreadCount() == 0 })
	time.Sleep(quietTime)
	assert.Len(t, fake.botComments(1), 1)
	assert.NoFileExists(t, claudeLog, "a description that cannot be proved started a turn")
}

func TestALongDescriptionCountsWhenAllBoardMembersAreTrusted(t *testing.T) {
	fake, server := newTestServer(t)
	fake.setMembers(botID, trustedID)
	fake.describe(1, trustedID, strings.Repeat("a long description ", 20))
	_, claudeLog := startDaemon(t, server)

	waitFor(t, "answer on card 1", func() bool { return len(fake.botComments(1)) == 1 })
	assert.Contains(t, fake.botComments(1)[0], "answer card 1")
	assert.FileExists(t, claudeLog)
	waitFor(t, "all notifications read", func() bool { return fake.unreadCount() == 0 })

	fake.setMembers(botID, trustedID, untrustedID)
	fake.describe(2, trustedID, strings.Repeat("another long description ", 20))
	waitFor(t, "notice on card 2", func() bool { return len(fake.botComments(2)) == 1 })
	assert.Contains(t, fake.botComments(2)[0], "longer than 200 characters")
}

func TestOnlyTrustedMembers(t *testing.T) {
	cfg := &config.Config{BotUserID: botID, TrustedUserIDs: []string{trustedID}}
	user := func(id string) fizzy.User { return fizzy.User{ID: id} }

	assert.True(t, onlyTrustedMembers(cfg, []fizzy.User{user(botID), user(trustedID)}))
	assert.True(t, onlyTrustedMembers(cfg, []fizzy.User{user(trustedID)}))
	assert.False(t, onlyTrustedMembers(cfg, []fizzy.User{user(botID), user(trustedID), user(untrustedID)}))
	assert.False(t, onlyTrustedMembers(cfg, []fizzy.User{user(botID)}), "a board with nobody else proves nothing")
	assert.False(t, onlyTrustedMembers(cfg, nil))
}

func TestAgentMessagesNeedTheTokenOfATurn(t *testing.T) {
	fake, server := newTestServer(t)
	d, _ := startDaemon(t, server)
	socket := d.cfg.SocketPath()
	message := func(token string) error {
		return ipc.Send(socket, ipc.Request{Op: ipc.OpMessage, Token: token, ToCard: 2, Text: "hello"})
	}

	assert.Error(t, message("invented"), "a request with an invented token was accepted")

	token, _, err := d.beginTurn(context.Background(), 1, 0)
	require.NoError(t, err)
	defer d.endTurn(token)
	assert.ErrorContains(t, message(token), "no agent has a session")

	fake.mention(2, trustedID)
	waitFor(t, "answer on card 2", func() bool { return len(fake.botComments(2)) == 1 })
	waitFor(t, "message accepted when the session is saved", func() bool { return message(token) == nil })

	lastHop, _, err := d.beginTurn(context.Background(), 1, maxAgentHops)
	require.NoError(t, err)
	defer d.endTurn(lastHop)
	assert.ErrorContains(t, message(lastHop), "limit")

	time.Sleep(quietTime)
	assert.Len(t, fake.botComments(2), 1, "a turn from an agent message posted a fallback comment")
}

// TestRealClaude runs one turn with the real claude command and the real MCP
// server. It uses the Claude login of the machine, so it is opt-in.
func TestRealClaude(t *testing.T) {
	if os.Getenv("FIZZY_CONNECTOR_REAL_CLAUDE") == "" {
		t.Skip("set FIZZY_CONNECTOR_REAL_CLAUDE=1 to run")
	}
	binary := filepath.Join(t.TempDir(), "fizzy-connector")
	out, err := exec.Command("go", "build", "-o", binary, "../../cmd/fizzy-connector").CombinedOutput()
	require.NoError(t, err, "%s", out)

	fake, server := newTestServer(t)
	fake.mention(7, trustedID)
	d, _ := startDaemonWith(t, server, "claude", binary)

	require.Eventually(t, func() bool { return len(fake.botComments(7)) > 0 }, 3*time.Minute, time.Second)
	log, err := os.ReadFile(filepath.Join(d.cfg.DataDir(), "logs", "card-7.log"))
	require.NoError(t, err)
	assert.Len(t, fake.botComments(7), 1)
	assert.Contains(t, string(log), "mcp__fizzy__reply", "the answer did not come from the reply tool")
	t.Logf("answer: %s", fake.botComments(7)[0])
}

// The slow fake claude takes a second, so that a stop can arrive during a turn.
const slowFakeClaude = `#!/bin/sh
echo "$@" >> "$FAKE_CLAUDE_LOG"
cat > /dev/null
echo '{"type":"system","subtype":"init"}'
sleep 1
echo '{"type":"result","is_error":false,"result":"slow answer"}'
`

func TestADrainLetsTheRunningTurnFinish(t *testing.T) {
	fake, server := newTestServer(t)
	script := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(script, []byte(slowFakeClaude), 0o755))
	d, claudeLog := startDaemonWith(t, server, script, "")

	fake.mention(7, trustedID)
	waitFor(t, "the turn to start", func() bool {
		log, _ := os.ReadFile(claudeLog)
		return len(log) > 0
	})
	d.Drain()
	fake.mention(8, trustedID)

	require.Eventually(t, func() bool { return len(fake.botComments(7)) == 1 }, 5*time.Second, 50*time.Millisecond)
	assert.Contains(t, fake.botComments(7)[0], "slow answer")
	assert.Empty(t, fake.botComments(8), "a mention after the drain started a turn")
}
