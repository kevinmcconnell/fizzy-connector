package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kevinmcconnell/fizzy-connector/internal/cable"
	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/ipc"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

const (
	maxAgentHops          = 3
	maxAgentMessageSize   = 20000
	maxQueueLength        = 20
	connectedPollInterval = 30 * time.Second
	ackReaction           = "👀"
)

type Daemon struct {
	cfg     *config.Config
	client  *fizzy.Client
	store   *store.Store
	botName string
	logger  *slog.Logger
	lock    *os.File
	runCtx  context.Context
	// drain ends when the daemon must start no more turns. The turns that
	// run finish first, unless the context of Run ends too.
	drain     context.Context
	stopDrain context.CancelFunc

	mcpCommand string

	slots          chan struct{}
	fetchSignal    chan struct{}
	cableConnected atomic.Bool
	etag           string

	mu            sync.Mutex
	running       map[int]bool
	turns         map[string]*turn
	approvals     map[int]int
	approvalGates map[int]chan struct{}
	pendingScans  map[int]bool
	workers       sync.WaitGroup
}

// turn is one running Claude turn. Its token authenticates the MCP server of
// that turn, and binds each request to the card and the life of the turn.
type turn struct {
	card      int
	hops      int
	ctx       context.Context
	cancel    context.CancelFunc
	tokenFile string
	replied   bool

	// consumed counts the items of the queue that this turn has: the items
	// it started with, and the items that it got after a tool call.
	// presentedUpTo is the time of the newest comment that it has seen, and
	// humanRequest reports an item from a person among them. requestedAt is
	// when the last item from a person arrived during the turn, and
	// lastCommentAt is when Claude last posted on the card.
	consumed      int
	presentedUpTo time.Time
	humanRequest  bool
	requestedAt   time.Time
	lastCommentAt time.Time
	noteAskedAt   time.Time
	// checkMu serializes the checks of a turn: tool calls of subagents can
	// run at the same time.
	checkMu sync.Mutex
}

// answered reports that Claude replied, and that it posted again after the
// last request that arrived during the turn.
func (t *turn) answered() bool {
	return t.replied && !t.lastCommentAt.Before(t.requestedAt)
}

func New(cfg *config.Config, logger *slog.Logger) (*Daemon, error) {
	if err := cfg.PrepareDataDir(); err != nil {
		return nil, err
	}
	cardStore, err := store.Open(cfg.DataDir())
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Daemon{
		mcpCommand:    executable,
		cfg:           cfg,
		client:        fizzy.NewClient(cfg.BaseURL, cfg.AccountSlug, cfg.Token),
		store:         cardStore,
		logger:        logger,
		slots:         make(chan struct{}, cfg.MaxConcurrent),
		fetchSignal:   make(chan struct{}, 1),
		running:       map[int]bool{},
		turns:         map[string]*turn{},
		approvals:     map[int]int{},
		approvalGates: map[int]chan struct{}{},
		pendingScans:  map[int]bool{},
	}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	if err := d.identify(ctx); err != nil {
		return err
	}

	listener, err := d.listen()
	if err != nil {
		return err
	}
	defer listener.Close()
	d.runCtx = ctx
	d.mu.Lock()
	d.drain, d.stopDrain = context.WithCancel(ctx)
	d.mu.Unlock()
	defer d.stopDrain()
	go ipc.Serve(listener, d.handleIPC)

	if err := d.resumeQueues(ctx); err != nil {
		return err
	}
	go d.watchCable(d.drain)
	go d.sweepLogs(d.drain)

	d.logger.Info("watching for mentions", "permission_mode", d.cfg.PermissionMode, "user", d.botName, "fizzy", d.cfg.BaseURL, "repo", d.cfg.Repo)
	d.fetchLoop(d.drain)

	if ctx.Err() == nil {
		d.logger.Info("stopping: the turns that run finish first. Stop again to end them now", "turns", d.activeTurns())
	}
	d.workers.Wait()
	return nil
}

// Drain makes the daemon stop after the turns that run now. Mentions stay
// unread in Fizzy for the next start.
func (d *Daemon) Drain() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopDrain != nil {
		d.stopDrain()
	}
}

func (d *Daemon) activeTurns() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.turns)
}

func (d *Daemon) identify(ctx context.Context) error {
	identity, err := d.client.Identity(ctx)
	if err != nil {
		return fmt.Errorf("fizzy login: %w", err)
	}
	for _, account := range identity.Accounts {
		if account.User.ID == d.cfg.BotUserID {
			d.botName = account.User.Name
			return nil
		}
	}
	return errors.New("the token does not belong to bot_user_id: run `fizzy-connector init` again")
}

func (d *Daemon) listen() (net.Listener, error) {
	lock, err := os.OpenFile(d.cfg.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("a fizzy-connector daemon is already running for this Fizzy account")
	}
	d.lock = lock

	path := d.cfg.SocketPath()
	if err := config.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	os.Remove(path)
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return listener, os.Chmod(path, 0o600)
}

// beginTurn makes the token of a turn, and writes it to a file that only this
// user can read. The MCP server of the turn gets the path, so the token is
// not on a command line.
func (d *Daemon) beginTurn(ctx context.Context, card, hops int) (string, *turn, error) {
	raw := make([]byte, 24)
	rand.Read(raw)
	token := hex.EncodeToString(raw)

	dir := filepath.Join(d.cfg.DataDir(), "turns")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	tokenFile := filepath.Join(dir, token[:16])
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return "", nil, err
	}

	turnCtx, cancel := context.WithCancel(ctx)
	t := &turn{card: card, hops: hops, ctx: turnCtx, cancel: cancel, tokenFile: tokenFile, lastCommentAt: time.Now(), requestedAt: time.Now()}
	d.mu.Lock()
	d.turns[token] = t
	d.mu.Unlock()
	return token, t, nil
}

// endTurn revokes the token and stops the requests of the turn that still
// wait, for example a permission question. It returns the turn as it ended,
// or nil when the token is not known.
func (d *Daemon) endTurn(token string) *turn {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.turns[token]
	if t == nil {
		return nil
	}
	delete(d.turns, token)
	t.cancel()
	os.Remove(t.tokenFile)
	return t
}

func (d *Daemon) turnFor(token string) *turn {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.turns[token]
}

func (d *Daemon) resumeQueues(ctx context.Context) error {
	states, err := d.store.All()
	if err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(d.cfg.DataDir(), "turns"))
	for _, state := range states {
		if state.NeedsScan {
			d.pendingScans[state.Number] = true
		}
		if len(state.Queue) > 0 || state.PendingReply != "" {
			d.logger.Info("continuing queued work", "card", state.Number)
			d.kick(state.Number)
		}
	}
	return nil
}

func (d *Daemon) signalFetch() {
	select {
	case d.fetchSignal <- struct{}{}:
	default:
	}
}

func (d *Daemon) fetchLoop(ctx context.Context) {
	for {
		if err := errors.Join(d.rescanPending(ctx), d.fetch(ctx)); err != nil && ctx.Err() == nil {
			d.logger.Warn("fetch failed", "error", err)
			d.etag = ""
		}

		interval := d.cfg.PollInterval.Duration
		if d.cableConnected.Load() {
			interval = connectedPollInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-d.fetchSignal:
		case <-time.After(interval):
		}
	}
}

func (d *Daemon) fetch(ctx context.Context) error {
	notifications, etag, err := d.client.Notifications(ctx, d.etag)
	if errors.Is(err, fizzy.ErrNotModified) {
		return nil
	}
	if err != nil {
		return err
	}
	d.etag = etag

	var failures []error
	for _, notification := range notifications {
		if notification.Read {
			continue
		}
		if err := d.handleNotification(ctx, notification); err != nil {
			failures = append(failures, fmt.Errorf("card %d: %w", notification.Card.Number, err))
		}
	}
	return errors.Join(failures...)
}

// handleNotification examines the card of each notification, of any type:
// Fizzy keeps one notification for each card and replaces its source, so a
// later event can hide a mention. A mention can also arrive between the scan
// and the acknowledgement, so the card is scanned again after it.
func (d *Daemon) handleNotification(ctx context.Context, notification fizzy.Notification) error {
	number := notification.Card.Number

	var evidence *mentionEvidence
	if notification.IsMention() {
		evidence = &mentionEvidence{mentioner: notification.Creator, body: notification.Body}
	}
	if err := d.checkCard(ctx, number, evidence); err != nil {
		if !errors.Is(err, errCardGone) {
			return err
		}
		d.logger.Warn("the card of a notification is deleted or not accessible: notification dropped", "card", number)
		return d.client.MarkNotificationRead(ctx, notification.ID)
	}

	// The second scan must not be lost: after the acknowledgement, Fizzy
	// does not show this notification again. The card keeps a mark until a
	// scan is successful.
	if err := d.setNeedsScan(number, true); err != nil {
		return err
	}
	if err := d.client.MarkNotificationRead(ctx, notification.ID); err != nil {
		return err
	}
	return d.rescan(ctx, number)
}

func (d *Daemon) setNeedsScan(number int, needed bool) error {
	_, err := d.store.Update(number, func(state *store.CardState) { state.NeedsScan = needed })
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if needed {
		d.pendingScans[number] = true
	} else {
		delete(d.pendingScans, number)
	}
	return nil
}

func (d *Daemon) rescan(ctx context.Context, number int) error {
	if err := d.checkCard(ctx, number, nil); err != nil && !errors.Is(err, errCardGone) {
		return err
	}
	return d.setNeedsScan(number, false)
}

func (d *Daemon) rescanPending(ctx context.Context) error {
	d.mu.Lock()
	var numbers []int
	for number := range d.pendingScans {
		numbers = append(numbers, number)
	}
	d.mu.Unlock()

	var failures []error
	for _, number := range numbers {
		if err := d.rescan(ctx, number); err != nil {
			failures = append(failures, fmt.Errorf("card %d: %w", number, err))
		}
	}
	return errors.Join(failures...)
}

var errCardGone = errors.New("the card is deleted or not accessible")

// checkCard records the new mentions on a card in its queue.
func (d *Daemon) checkCard(ctx context.Context, number int, evidence *mentionEvidence) error {
	card, err := d.client.Card(ctx, number)
	var apiErr *fizzy.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusForbidden) {
		return errCardGone
	}
	if err != nil {
		return err
	}
	comments, err := d.client.Comments(ctx, number)
	if err != nil {
		return err
	}
	state, err := d.store.Get(number)
	if err != nil {
		return err
	}

	trustedBoard := false
	if longDescriptionMention(d.cfg, card, evidence) {
		members, err := d.client.BoardMembers(ctx, card.Board.ID)
		if err != nil {
			return err
		}
		trustedBoard = onlyTrustedMembers(d.cfg, members)
		if id := descriptionID("long-description", card.Description); !trustedBoard && !state.IsHandled(id) {
			if err := d.explainLongDescription(ctx, number, id); err != nil {
				return err
			}
		}
	}

	triggers := findTriggers(d.cfg, state, card, comments, evidence, trustedBoard, d.approvalPending(number))
	if len(triggers) == 0 {
		return nil
	}

	_, err = d.store.Update(number, func(state *store.CardState) {
		for _, trigger := range triggers {
			if state.IsHandled(trigger.item.TriggerID) {
				continue
			}
			state.Handled = append(state.Handled, trigger.item.TriggerID)
			if trigger.trusted {
				state.Queue = append(state.Queue, trigger.item)
			}
		}
	})
	if err != nil {
		return err
	}

	queued := false
	for _, trigger := range triggers {
		if !trigger.trusted {
			d.logger.Warn("ignored a mention from a user that is not trusted",
				"card", number, "user", trigger.author.Name, "user_id", trigger.author.ID)
			continue
		}
		queued = true
		d.logger.Info("mention received", "card", number, "from", trigger.author.Name)
		d.acknowledge(ctx, number, trigger.item)
	}
	if queued {
		d.kick(number)
	}
	return nil
}

const longDescriptionNotice = "I see a mention in the description of this card. The description is longer than 200 characters, and this board has people who are not in my trusted list. All of them can edit a description, so I cannot confirm who wrote all of it. Mention me in a comment, and I will do the work."

func (d *Daemon) explainLongDescription(ctx context.Context, number int, id string) error {
	if _, err := d.client.CreateComment(ctx, number, fizzy.MarkdownToHTML(longDescriptionNotice)); err != nil {
		return err
	}
	_, err := d.store.Update(number, func(state *store.CardState) { state.Handled = append(state.Handled, id) })
	return err
}

func (d *Daemon) acknowledge(ctx context.Context, number int, item store.Item) {
	var err error
	if item.Kind == store.KindDescription {
		err = d.client.ReactToCard(ctx, number, ackReaction)
	} else {
		err = d.client.ReactToComment(ctx, number, item.TriggerID, ackReaction)
	}
	if err != nil {
		d.logger.Warn("acknowledge failed", "card", number, "error", err)
	}
}

func (d *Daemon) handleIPC(request ipc.Request) ipc.Response {
	t := d.turnFor(request.Token)
	if t == nil {
		return ipc.Response{Error: "this request does not belong to a turn that runs"}
	}

	switch request.Op {
	case ipc.OpReplied:
		d.noteComment(t, true)
		return ipc.Response{}
	case ipc.OpProgress:
		d.noteComment(t, false)
		return ipc.Response{}
	case ipc.OpCheck:
		return d.check(t)
	case ipc.OpMessage:
		if err := d.deliverAgentMessage(t, request); err != nil {
			return ipc.Response{Error: err.Error()}
		}
		return ipc.Response{}
	case ipc.OpApprove:
		return d.requestApproval(t, request)
	}
	return ipc.Response{Error: fmt.Sprintf("unknown operation %q", request.Op)}
}

// noteComment records a comment of Claude on the card. A turn that ended
// does not change: the worker reads the turn after endTurn.
func (d *Daemon) noteComment(t *turn, isReply bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.ctx.Err() != nil {
		return
	}
	t.replied = t.replied || isReply
	t.lastCommentAt = time.Now()
}

func (d *Daemon) deliverAgentMessage(t *turn, request ipc.Request) error {
	if request.ToCard == t.card {
		return errors.New("you cannot send a message to your own card")
	}
	if t.ctx.Err() != nil {
		return errors.New("the turn ended")
	}
	d.mu.Lock()
	hops := t.hops
	d.mu.Unlock()
	if hops >= maxAgentHops {
		return fmt.Errorf("message refused: this chain of agent messages reached the limit of %d; reply on your card instead", maxAgentHops)
	}
	if len(request.Text) > maxAgentMessageSize {
		return fmt.Errorf("message refused: the limit is %d characters", maxAgentMessageSize)
	}
	target, err := d.store.Get(request.ToCard)
	if err != nil {
		return err
	}
	if !target.SessionStarted {
		return fmt.Errorf("no agent has a session for card #%d; use read_card to read that card", request.ToCard)
	}
	if len(target.Queue) >= maxQueueLength {
		return fmt.Errorf("message refused: the agent of card #%d has too much queued work", request.ToCard)
	}

	_, err = d.store.Update(request.ToCard, func(state *store.CardState) {
		state.Queue = append(state.Queue, store.Item{
			Kind:     store.KindAgentMessage,
			FromCard: t.card,
			Text:     request.Text,
			Hops:     hops + 1,
		})
	})
	if err != nil {
		return err
	}
	d.logger.Info("agent message queued", "from_card", t.card, "to_card", request.ToCard)
	d.kick(request.ToCard)
	return nil
}

func (d *Daemon) watchCable(ctx context.Context) {
	if d.cfg.SessionToken() == "" {
		d.logger.Info("no websocket login: using the timer only (run `fizzy-connector login` for real-time)",
			"interval", d.cfg.PollInterval.Duration)
	}

	backoff := time.Second
	for ctx.Err() == nil {
		if token := d.cfg.SessionToken(); token != "" {
			err := d.listenCable(ctx, token, func() { backoff = time.Second })
			d.cableConnected.Store(false)
			if ctx.Err() != nil {
				return
			}
			d.logger.Warn("websocket not connected: using the timer", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
}

func (d *Daemon) listenCable(ctx context.Context, token string, onReady func()) error {
	streamName, err := d.client.NotificationStreamName(ctx, token)
	if err != nil {
		return err
	}
	subscription := cable.Subscription{
		BaseURL:      d.cfg.BaseURL,
		AccountSlug:  d.cfg.AccountSlug,
		SessionToken: token,
		StreamName:   streamName,
	}
	return subscription.Listen(ctx, func() {
		onReady()
		d.cableConnected.Store(true)
		d.logger.Info("websocket connected: mentions arrive in real time")
		d.signalFetch()
	}, d.signalFetch)
}
