package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kevinmcconnell/fizzy-connector/internal/claude"
	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/daemon"
	"github.com/kevinmcconnell/fizzy-connector/internal/mcpserver"
	"github.com/kevinmcconnell/fizzy-connector/internal/setup"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "fizzy-connector:", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:           "fizzy-connector",
		Short:         "Connect Fizzy to Claude Code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath(), "path of the config file")

	loadConfig := func() (*config.Config, error) {
		cfg, err := config.Load(configPath)
		if err != nil {
			return nil, err
		}
		return cfg, cfg.PrepareDataDir()
	}

	withConfig := func(run func(cmd *cobra.Command, cfg *config.Config, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			return run(cmd, cfg, args)
		}
	}

	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "Make the config file, and make or connect the Fizzy user for Claude",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setup.Init(cmd.Context(), configPath)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "login",
		Short: "Log in for real-time mentions through the websocket (optional)",
		Args:  cobra.NoArgs,
		RunE: withConfig(func(cmd *cobra.Command, cfg *config.Config, _ []string) error {
			return setup.Login(cmd.Context(), cfg)
		}),
	})

	root.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Check the configuration",
		Args:  cobra.NoArgs,
		RunE: withConfig(func(cmd *cobra.Command, cfg *config.Config, _ []string) error {
			return setup.Doctor(cmd.Context(), cfg)
		}),
	})

	root.AddCommand(newRunCommand(withConfig))
	root.AddCommand(newMCPCommand(withConfig))

	root.AddCommand(&cobra.Command{
		Use:   "sessions [card]",
		Short: "List the card sessions with their cost, or the turns of one card",
		Args:  cobra.MaximumNArgs(1),
		RunE: withConfig(func(_ *cobra.Command, cfg *config.Config, args []string) error {
			if len(args) == 1 {
				return showTurns(cfg, args[0])
			}
			return listSessions(cfg)
		}),
	})

	root.AddCommand(&cobra.Command{
		Use:   "reset <card>",
		Short: "Start a new session for a card at its next turn, with the card content and without the old history",
		Args:  cobra.ExactArgs(1),
		RunE: withConfig(func(_ *cobra.Command, cfg *config.Config, args []string) error {
			return resetSession(cfg, args[0])
		}),
	})

	// The daemon sets this command as the PostToolUse hook of each turn. It
	// is not for direct use.
	root.AddCommand(&cobra.Command{
		Use:    "turn-hook",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return claude.TurnHook(time.Now(), os.Getenv, os.Stdin, os.Stdout)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "attach <card>",
		Short: "Open the session of a card in interactive Claude Code",
		Args:  cobra.ExactArgs(1),
		RunE: withConfig(func(_ *cobra.Command, cfg *config.Config, args []string) error {
			return attach(cfg, args[0])
		}),
	})

	return root
}

type configCommand func(func(*cobra.Command, *config.Config, []string) error) func(*cobra.Command, []string) error

func newRunCommand(withConfig configCommand) *cobra.Command {
	var permissionMode string

	command := &cobra.Command{
		Use:   "run",
		Short: "Watch Fizzy for mentions and run the Claude sessions",
		Args:  cobra.NoArgs,
		RunE: withConfig(func(cmd *cobra.Command, cfg *config.Config, _ []string) error {
			if permissionMode != "" {
				if err := cfg.SetPermissionMode(permissionMode); err != nil {
					return err
				}
			}
			d, err := daemon.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err != nil {
				return err
			}
			// The first signal lets the turns that run finish. The second
			// stops them.
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			signals := make(chan os.Signal, 2)
			signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(signals)
			go func() {
				<-signals
				d.Drain()
				<-signals
				stop()
			}()
			return d.Run(ctx)
		}),
	}
	command.Flags().StringVar(&permissionMode, "permission-mode", "", "override permission_mode of the config: auto, acceptEdits or bypassPermissions")
	return command
}

// The daemon starts the mcp command for each turn. It is not for direct use.
func newMCPCommand(withConfig configCommand) *cobra.Command {
	var card int
	var socket, tokenFile string

	command := &cobra.Command{
		Use:    "mcp",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: withConfig(func(cmd *cobra.Command, cfg *config.Config, _ []string) error {
			token := ""
			if tokenFile != "" {
				raw, err := os.ReadFile(tokenFile)
				if err != nil {
					return fmt.Errorf("read the token of the turn: %w", err)
				}
				token = string(raw)
			}
			return mcpserver.New(cfg, card, socket, token).Run(cmd.Context())
		}),
	}
	command.Flags().IntVar(&card, "card", 0, "card number")
	command.Flags().StringVar(&socket, "socket", "", "daemon socket")
	command.Flags().StringVar(&tokenFile, "token-file", "", "file with the token of the turn")
	command.MarkFlagRequired("card")
	return command
}

func parseCardNumber(arg string) (int, error) {
	number, err := strconv.Atoi(arg)
	if err != nil || number < 1 {
		return 0, fmt.Errorf("%q is not a card number", arg)
	}
	return number, nil
}

func listSessions(cfg *config.Config) error {
	cards, err := store.Open(cfg.DataDir())
	if err != nil {
		return err
	}
	states, err := cards.All()
	if err != nil {
		return err
	}

	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	total := 0.0
	fmt.Fprintln(table, "CARD\tSESSION\tQUEUED\tTURNS\tCONTEXT\tLAST COST\tTOTAL COST (EST.)\tLAST TURN")
	for _, state := range states {
		lastTurn, lastCost, context := "-", "-", "-"
		if !state.LastTurnAt.IsZero() {
			lastTurn = state.LastTurnAt.Local().Format("2006-01-02 15:04")
		}
		if len(state.Turns) > 0 {
			last := state.Turns[len(state.Turns)-1]
			lastCost = fmt.Sprintf("$%.3f", last.CostUSD)
			context = thousands(last.ContextTokens)
		}
		total += state.TotalCostUSD
		fmt.Fprintf(table, "#%d\t%s\t%d\t%d\t%s\t%s\t$%.2f\t%s\n", state.Number, state.SessionID, len(state.Queue), state.TurnCount, context, lastCost, state.TotalCostUSD, lastTurn)
	}
	fmt.Fprintf(table, "\t\t\t\t\t\t$%.2f\t\n", total)
	return table.Flush()
}

func showTurns(cfg *config.Config, cardArg string) error {
	number, err := parseCardNumber(cardArg)
	if err != nil {
		return err
	}
	cards, err := store.Open(cfg.DataDir())
	if err != nil {
		return err
	}
	state, err := cards.Get(number)
	if err != nil {
		return err
	}

	table := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "TURN AT\tSECONDS\tSESSION\tAPI CALLS\tINPUT\tCACHE WRITE\tCACHE READ\tOUTPUT\tCONTEXT\tCOST (EST.)")
	for _, turn := range state.Turns {
		session := "new"
		if turn.Resumed {
			session = "resumed"
		}
		fmt.Fprintf(table, "%s\t%d\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t$%.3f\n", turn.At.Local().Format("2006-01-02 15:04"),
			turn.Seconds, session, turn.APICalls, turn.InputTokens, turn.CacheWriteTokens, turn.CacheReadTokens, turn.OutputTokens,
			thousands(turn.ContextTokens), turn.CostUSD)
	}
	return table.Flush()
}

// thousands shows a token count as "258K".
func thousands(count int) string {
	if count == 0 {
		return "-"
	}
	return fmt.Sprintf("%dK", (count+500)/1000)
}

func resetSession(cfg *config.Config, cardArg string) error {
	number, err := parseCardNumber(cardArg)
	if err != nil {
		return err
	}
	cards, err := store.Open(cfg.DataDir())
	if err != nil {
		return err
	}
	state, err := cards.Update(number, func(state *store.CardState) { state.ResetSession() })
	if err != nil {
		return err
	}
	fmt.Printf("card #%d starts a new session %s at its next turn\n", number, state.SessionID)
	return nil
}

func attach(cfg *config.Config, cardArg string) error {
	number, err := parseCardNumber(cardArg)
	if err != nil {
		return err
	}
	cards, err := store.Open(cfg.DataDir())
	if err != nil {
		return err
	}
	state, err := cards.Get(number)
	if err != nil {
		return err
	}
	if !state.SessionStarted {
		return fmt.Errorf("card #%d has no session", number)
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	turn := claude.Turn{
		SessionID:  state.SessionID,
		MCPCommand: executable,
		MCPArgs:    daemon.MCPArgs(cfg.Path, number, "", ""),
		Env:        cfg.ClaudeEnv(),
	}
	return turn.Attach(cfg.ClaudePath, cfg.Repo)
}
