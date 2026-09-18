// Package setup has the interactive commands: init, login and doctor.
package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
)

var errCancelled = errors.New("cancelled")

// prompter asks questions. On a terminal it is a line editor, so that the
// cursor keys, backspace and Ctrl-C work. With piped input it reads lines.
type prompter struct {
	out      io.Writer
	lines    *bufio.Reader
	terminal *term.Terminal
	restore  func()
}

// newPrompter puts the terminal in raw mode. A read from the terminal does
// not see the context, so when the context ends (SIGTERM) the prompter
// restores the terminal and stops the program.
func newPrompter(ctx context.Context) *prompter {
	p := makePrompter()
	closed := make(chan struct{})
	restore := p.restore
	p.restore = func() {
		select {
		case <-closed:
		default:
			close(closed)
		}
		restore()
	}
	go func() {
		select {
		case <-ctx.Done():
			restore()
			fmt.Fprintln(os.Stderr, "\nfizzy-connector: stopped")
			os.Exit(1)
		case <-closed:
		}
	}()
	return p
}

func makePrompter() *prompter {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return &prompter{out: os.Stdout, lines: bufio.NewReader(os.Stdin), restore: func() {}}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return &prompter{out: os.Stdout, lines: bufio.NewReader(os.Stdin), restore: func() {}}
	}

	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}, "")
	return &prompter{out: terminal, terminal: terminal, restore: func() { term.Restore(fd, state) }}
}

func (p *prompter) close() { p.restore() }

func (p *prompter) ask(question, fallback string) (string, error) {
	return p.read(question, fallback, false)
}

func (p *prompter) askSecret(question string) (string, error) {
	for {
		answer, err := p.read(question, "", true)
		if err != nil || answer != "" {
			return answer, err
		}
	}
}

func (p *prompter) read(question, fallback string, secret bool) (string, error) {
	prompt := question + ": "
	if fallback != "" {
		prompt = fmt.Sprintf("%s [%s]: ", question, fallback)
	}

	var line string
	var err error
	switch {
	case p.terminal == nil:
		fmt.Fprint(p.out, prompt)
		line, err = p.lines.ReadString('\n')
		if err != nil && line != "" {
			err = nil
		}
	case secret:
		line, err = p.terminal.ReadPassword(prompt)
	default:
		p.terminal.SetPrompt(prompt)
		line, err = p.terminal.ReadLine()
	}
	if errors.Is(err, io.EOF) {
		fmt.Fprintln(p.out)
		return "", errCancelled
	}
	if err != nil {
		return "", err
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return fallback, nil
}

func (p *prompter) askRequired(question, fallback string) (string, error) {
	for {
		answer, err := p.ask(question, fallback)
		if err != nil || answer != "" {
			return answer, err
		}
	}
}

func Init(ctx context.Context, path string) error {
	p := newPrompter(ctx)
	defer p.close()
	cfg := config.Defaults()
	if existing, err := config.Load(path); err == nil {
		cfg = existing
		fmt.Fprintf(p.out, "Updating %s\n\n", path)
	}

	var err error
	if cfg.BaseURL, err = p.askRequired("Fizzy URL", orDefault(cfg.BaseURL, "https://app.fizzy.do")); err != nil {
		return err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	fmt.Fprintln(p.out, "\nClaude needs its own Fizzy user. How do you want to connect it?")
	fmt.Fprintln(p.out, "  1. Make a new user from an invite link (Account settings > Invite people)")
	fmt.Fprintln(p.out, "  2. Log in to an existing user with its email address")
	fmt.Fprintln(p.out, "  3. Enter a personal access token of an existing user")
	method := ""
	for method != "1" && method != "2" && method != "3" {
		if method, err = p.askRequired("Selection", "1"); err != nil {
			return err
		}
	}

	var account *fizzy.Account
	sessionToken := ""
	switch method {
	case "1":
		account, sessionToken, err = createBotUser(ctx, p, cfg)
	case "2":
		account, sessionToken, err = loginBotUser(ctx, p, cfg)
	case "3":
		if cfg.Token, err = p.askSecret("Access token (Read + Write) of the Claude user (not shown)"); err == nil {
			account, err = chooseAccount(ctx, p, fizzy.NewClient(cfg.BaseURL, "", cfg.Token), "")
		}
	}
	if err != nil {
		return err
	}
	cfg.AccountSlug = strings.Trim(account.Slug, "/")
	cfg.BotUserID = account.User.ID
	fmt.Fprintf(p.out, "\nClaude uses the Fizzy user %q in the account %q.\n\n", account.User.Name, account.Name)

	client := fizzy.NewClient(cfg.BaseURL, cfg.AccountSlug, cfg.Token)
	if cfg.TrustedUserIDs, err = chooseTrustedUsers(ctx, p, client, cfg.BotUserID); err != nil {
		return err
	}

	for {
		if cfg.Repo, err = p.askRequired("Directory where the Claude sessions work", cfg.Repo); err != nil {
			return err
		}
		if cfg.Repo, err = filepath.Abs(expandHome(cfg.Repo)); err != nil {
			return err
		}
		if info, statErr := os.Stat(cfg.Repo); statErr == nil && info.IsDir() {
			break
		}
		fmt.Fprintf(p.out, "%s is not a directory.\n", cfg.Repo)
	}

	if err := cfg.Write(path); err != nil {
		return err
	}
	if sessionToken != "" {
		if err := cfg.SaveSessionToken(sessionToken); err != nil {
			return err
		}
		fmt.Fprintln(p.out, "\nSaved the websocket login: mentions arrive in real time.")
	}

	fmt.Fprintf(p.out, "\nWrote %s\n\n", path)
	fmt.Fprintf(p.out, "In Fizzy, give %q access to the boards where people will mention it.\n\nNext steps:\n", account.User.Name)
	if sessionToken == "" {
		fmt.Fprintln(p.out, "  fizzy-connector login    (optional: real-time mentions through the websocket)")
	}
	fmt.Fprintln(p.out, "  fizzy-connector doctor")
	fmt.Fprintln(p.out, "  fizzy-connector run")
	return nil
}

var inviteLinkPattern = regexp.MustCompile(`/(\d+)/join/([\w-]+)`)

func createBotUser(ctx context.Context, p *prompter, cfg *config.Config) (*fizzy.Account, string, error) {
	var slug, code string
	for {
		link, err := p.askRequired("Invite link", "")
		if err != nil {
			return nil, "", err
		}
		if match := inviteLinkPattern.FindStringSubmatch(link); match != nil {
			slug, code = match[1], match[2]
			break
		}
		fmt.Fprintln(p.out, "An invite link looks like https://fizzy.example/1234567/join/AbCd-EfGh-IjKl")
	}
	fmt.Fprintln(p.out, "The user needs a mailbox that you can read: Fizzy sends the login code to it.")
	email, err := p.askRequired("Email address for the new user", "")
	if err != nil {
		return nil, "", err
	}
	name, err := p.askRequired("Name of the new user", "Claude")
	if err != nil {
		return nil, "", err
	}

	if err := fizzy.NewClient(cfg.BaseURL, slug, "").Join(ctx, code, email); err != nil {
		return nil, "", fmt.Errorf("join the account: %w", err)
	}
	sessionToken, err := magicLinkLogin(ctx, p, cfg.BaseURL, email)
	if err != nil {
		return nil, "", err
	}

	session := fizzy.NewSessionClient(cfg.BaseURL, slug, sessionToken)
	if err := session.VerifyUser(ctx); err != nil {
		return nil, "", fmt.Errorf("verify the user: %w", err)
	}
	account, err := chooseAccount(ctx, p, session, slug)
	if err != nil {
		return nil, "", err
	}
	if err := session.RenameUser(ctx, account.User.ID, name); err != nil {
		return nil, "", fmt.Errorf("set the user name: %w", err)
	}
	account.User.Name = name

	cfg.Token, err = session.CreateAccessToken(ctx, "fizzy-connector")
	return account, sessionToken, err
}

func loginBotUser(ctx context.Context, p *prompter, cfg *config.Config) (*fizzy.Account, string, error) {
	email, err := p.askRequired("Email address of the Claude user", "")
	if err != nil {
		return nil, "", err
	}
	sessionToken, err := magicLinkLogin(ctx, p, cfg.BaseURL, email)
	if err != nil {
		return nil, "", err
	}
	account, err := chooseAccount(ctx, p, fizzy.NewSessionClient(cfg.BaseURL, "", sessionToken), "")
	if err != nil {
		return nil, "", err
	}

	session := fizzy.NewSessionClient(cfg.BaseURL, strings.Trim(account.Slug, "/"), sessionToken)
	cfg.Token, err = session.CreateAccessToken(ctx, "fizzy-connector")
	return account, sessionToken, err
}

func magicLinkLogin(ctx context.Context, p *prompter, baseURL, email string) (string, error) {
	client := fizzy.NewClient(baseURL, "", "")
	link, err := client.RequestMagicLink(ctx, email)
	if err != nil {
		return "", fmt.Errorf("request the login code: %w", err)
	}
	code, err := p.askRequired("Code from the Fizzy email to "+email, link.DevelopmentCode)
	if err != nil {
		return "", err
	}
	token, err := client.SubmitMagicLink(ctx, link.PendingToken, strings.ToUpper(code))
	if err != nil {
		return "", fmt.Errorf("the code was not accepted by Fizzy: %w", err)
	}
	return token, nil
}

// chooseAccount returns the account with the slug, or asks when the slug is
// empty and the user has more than one account.
func chooseAccount(ctx context.Context, p *prompter, client *fizzy.Client, slug string) (*fizzy.Account, error) {
	identity, err := client.Identity(ctx)
	if err != nil {
		return nil, fmt.Errorf("the login was not accepted by Fizzy: %w", err)
	}
	if slug != "" {
		for i, account := range identity.Accounts {
			if strings.Trim(account.Slug, "/") == slug {
				return &identity.Accounts[i], nil
			}
		}
		return nil, errors.New("the user is not in the account of the invite link")
	}

	switch len(identity.Accounts) {
	case 0:
		return nil, errors.New("this user is not in a Fizzy account")
	case 1:
		return &identity.Accounts[0], nil
	}

	fmt.Fprintln(p.out, "\nThis user is in more than one account:")
	for i, account := range identity.Accounts {
		fmt.Fprintf(p.out, "  %d. %s\n", i+1, account.Name)
	}
	for {
		answer, err := p.askRequired("Account number", "1")
		if err != nil {
			return nil, err
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(identity.Accounts) {
			return &identity.Accounts[n-1], nil
		}
	}
}

func chooseTrustedUsers(ctx context.Context, p *prompter, client *fizzy.Client, botUserID string) ([]string, error) {
	users, err := client.Users(ctx)
	if err != nil {
		return nil, err
	}
	var candidates []fizzy.User
	for _, user := range users {
		if user.Active && user.ID != botUserID && user.Role != "system" {
			candidates = append(candidates, user)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("the account has no other users: add yourself to the account first")
	}

	fmt.Fprintln(p.out, "A mention starts a Claude session with access to your machine.")
	fmt.Fprintln(p.out, "Select the users whose mentions Claude obeys:")
	for i, user := range candidates {
		fmt.Fprintf(p.out, "  %d. %s <%s>\n", i+1, user.Name, user.EmailAddress)
	}
	for {
		answer, err := p.askRequired("Trusted users (numbers, comma separated)", "")
		if err != nil {
			return nil, err
		}
		var ids []string
		valid := true
		for field := range strings.SplitSeq(answer, ",") {
			n, convErr := strconv.Atoi(strings.TrimSpace(field))
			if convErr != nil || n < 1 || n > len(candidates) {
				valid = false
				break
			}
			ids = append(ids, candidates[n-1].ID)
		}
		if valid {
			fmt.Fprintln(p.out)
			return ids, nil
		}
	}
}

func Login(ctx context.Context, cfg *config.Config) error {
	p := newPrompter(ctx)
	defer p.close()
	email, err := p.askRequired("Email address of the Claude user", botEmail(ctx, cfg))
	if err != nil {
		return err
	}
	token, err := magicLinkLogin(ctx, p, cfg.BaseURL, email)
	if err != nil {
		return err
	}
	if err := verifySessionUser(ctx, cfg, token); err != nil {
		return err
	}
	client := fizzy.NewClient(cfg.BaseURL, cfg.AccountSlug, "")
	if _, err := client.NotificationStreamName(ctx, token); err != nil {
		return err
	}
	if err := cfg.SaveSessionToken(token); err != nil {
		return err
	}
	fmt.Fprintln(p.out, "Login saved. The daemon now gets mentions in real time.")
	return nil
}

// verifySessionUser makes sure that the login is the Claude user. The
// websocket of a different user would give signals for the wrong person.
func verifySessionUser(ctx context.Context, cfg *config.Config, sessionToken string) error {
	identity, err := fizzy.NewSessionClient(cfg.BaseURL, cfg.AccountSlug, sessionToken).Identity(ctx)
	if err != nil {
		return err
	}
	for _, account := range identity.Accounts {
		if account.User.ID == cfg.BotUserID {
			return nil
		}
	}
	return errors.New("this login is not the Claude user of the config: log in with the email address of that user")
}

func botEmail(ctx context.Context, cfg *config.Config) string {
	identity, err := fizzy.NewClient(cfg.BaseURL, cfg.AccountSlug, cfg.Token).Identity(ctx)
	if err != nil {
		return ""
	}
	for _, account := range identity.Accounts {
		if account.User.ID == cfg.BotUserID {
			return account.User.EmailAddress
		}
	}
	return ""
}

func Doctor(ctx context.Context, cfg *config.Config) error {
	client := fizzy.NewClient(cfg.BaseURL, cfg.AccountSlug, cfg.Token)
	failed := false
	check := func(name string, run func() (string, error)) {
		detail, err := run()
		if err != nil {
			failed = true
			fmt.Printf("FAIL  %s: %v\n", name, err)
			return
		}
		fmt.Printf("ok    %s: %s\n", name, detail)
	}

	check("Fizzy token", func() (string, error) {
		identity, err := client.Identity(ctx)
		if err != nil {
			return "", err
		}
		for _, account := range identity.Accounts {
			if account.User.ID == cfg.BotUserID {
				return fmt.Sprintf("user %q in account %q", account.User.Name, account.Name), nil
			}
		}
		return "", errors.New("the token does not belong to bot_user_id")
	})
	check("Notifications", func() (string, error) {
		notifications, _, err := client.Notifications(ctx, "")
		return fmt.Sprintf("%d on the first page", len(notifications)), err
	})
	check("Board access", func() (string, error) {
		boards, err := client.Boards(ctx)
		if err != nil {
			return "", err
		}
		if len(boards) == 0 {
			return "", errors.New("the Claude user has access to no boards, so nobody can mention it")
		}
		names := make([]string, len(boards))
		for i, board := range boards {
			names[i] = board.Name
		}
		return strings.Join(names, ", "), nil
	})
	check("Trusted users", func() (string, error) {
		if len(cfg.TrustedUserIDs) == 0 {
			return "", errors.New("trusted_user_ids is empty: Claude ignores all mentions")
		}
		return strconv.Itoa(len(cfg.TrustedUserIDs)), nil
	})
	check("Work directory", func() (string, error) {
		info, err := os.Stat(cfg.Repo)
		if err == nil && !info.IsDir() {
			err = errors.New("not a directory")
		}
		return cfg.Repo, err
	})
	check("Permission mode", func() (string, error) {
		detail := cfg.PermissionMode
		if cfg.Approvals {
			detail += ", permission questions go to the card"
		} else {
			detail += ", actions that need a prompt are refused"
		}
		if cfg.PermissionMode == "bypassPermissions" {
			detail += "\nWARN  bypassPermissions lets a mention run any command on this machine: use it only in a container"
		}
		for _, dir := range cfg.AddDirs {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				return "", fmt.Errorf("add_dirs: %s is not a directory", dir)
			}
		}
		return detail, nil
	})
	check("Claude Code", func() (string, error) {
		version := exec.CommandContext(ctx, cfg.ClaudePath, "--version")
		version.Env = append(os.Environ(), cfg.ClaudeEnv()...)
		out, err := version.Output()
		detail := strings.TrimSpace(string(out))
		if cfg.ClaudeConfigDir != "" {
			detail += ", config in " + cfg.ClaudeConfigDir
		}
		return detail, err
	})
	check("Websocket login", func() (string, error) {
		token := cfg.SessionToken()
		if token == "" {
			return "not configured (optional): the daemon uses the timer", nil
		}
		_, err := client.NotificationStreamName(ctx, token)
		return "session cookie accepted", err
	})

	if failed {
		return errors.New("one or more checks failed")
	}
	return nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return path
}
