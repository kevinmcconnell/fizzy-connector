# fizzy-connector

Connects [Fizzy](https://github.com/basecamp/fizzy) to Claude Code. When a
trusted person @mentions the Claude user on a card, Claude answers with a
comment on that card.

- Each card is a separate Claude Code session with its own context. A later
  mention on the same card continues that session.
- The reply tool of a session is bound to its card, so an answer cannot go to a
  different card.
- All connections are outbound. It works with self-hosted Fizzy and with
  fizzy.do, and it needs no public URL and no webhooks.

## Setup

```sh
go install ./cmd/fizzy-connector
fizzy-connector init
fizzy-connector doctor
fizzy-connector run
```

Claude needs its own Fizzy user. `init` can connect it in three ways:

1. **Make a new user from an invite link.** Copy the link from Account
   settings > Invite people. `init` asks for an email address and a name,
   joins the account, and asks for the login code that Fizzy sends to that
   mailbox. It then makes the access token and saves the websocket login.
2. **Log in to an existing user** with its email address and a login code.
3. **Enter a personal access token** (Read + Write) that you made in the
   Fizzy UI. Run `fizzy-connector login` afterwards if you want real-time
   mentions.

`init` also asks which users Claude obeys, and for the work directory.

After `init`, give the Claude user access to the boards where people will
mention it (board settings in Fizzy). `doctor` lists the boards that it can
use. Claude can read and change only those boards.

`init` writes `~/.config/fizzy-connector/config.toml`. State (sessions, queues,
logs) is in `~/.local/state/fizzy-connector`, in one directory for each Fizzy
account and Claude user.

## How it gets mentions

The daemon reads the notifications of the Claude user from the Fizzy API. This
is the source of truth. Without `login`, it reads them each `poll_interval`
(default 3 s).

`login` does the Fizzy magic-link login and stores a session cookie. With it,
the daemon also listens on the same websocket that the Fizzy page uses. A
notification then starts a fetch immediately, and the timer decreases to 30 s.
The websocket is not a public API. If it stops working after a Fizzy update,
the daemon continues with the timer and logs a warning.

## Sessions and limits

A session is a transcript on disk, one for each card. Mentions on one card go
to its session in sequence. Mentions that arrive while a turn runs go
together into the next turn, and get one answer.

A `claude` process runs only for one turn. `max_concurrent` limits the processes, not the sessions:
with a limit of 4 and mentions on 10 cards, 4 turns run and 6 cards wait in a
queue. Each mention gets a 👀 reaction immediately. `turn_timeout` stops a turn
that runs too long.

```sh
fizzy-connector sessions      # the card sessions, with turn counts and cost
fizzy-connector sessions 42   # the turns of card 42, with token counts
fizzy-connector attach 42     # open the session of card 42 interactively
```

An attached session has the Fizzy tools, but not `message_card_agent` and not
the permission questions in Fizzy: you are at the terminal to answer them.

The connector records the usage that Claude Code reports for each turn. The
cost is an estimate at API prices. With a subscription you do not pay it, but
the token counts show what uses your limits. A high "cache write" value on a
resumed turn means that the prompt cache was cold, and the full history of
the card was sent as new input.

Logs of each turn are in `~/.local/state/fizzy-connector/<account>/logs/card-N.log`.

## Security

A mention starts Claude Code on your machine, in the `repo` directory. Only
the users in `trusted_user_ids` can do this. Mentions from other users are
ignored and logged.

**Who made a mention.** The author of a comment comes from the Fizzy API. A
card description has no author, because all people with board access can edit
it. So a mention in a description counts only when the Fizzy notification
proves the complete text: it names a trusted user as the person who made the
mention, and its text is the same as the description. Fizzy cuts that text at
200 characters. For a longer description, Claude posts a comment that asks for
a mention in a comment. The turn uses the text that was proved, and not a
later state of the description. A comment is the reliable way to give work to
Claude.

**What a session sees.** Each prompt is a JSON document that the connector
makes. It lists the requests of the turn, and it has only the comments from
trusted people. Titles, names and text are string values in that document, so
they cannot imitate its structure. The system prompt has no text from Fizzy.
When a task needs the full discussion, Claude reads the card with `read_card`,
which marks each author as trusted or not trusted. Claude is told to do only
the actions that the author of a request asked for, and to report other
proposed steps in its reply. These are instructions in a prompt, not limits in
code.

**The hard limits** are the permission mode, `allowed_tools`, and the boards
that the Claude user can access in Fizzy.

**What the connector does not protect.** The sessions run as your OS user. A
session that can run shell commands can thus read the config file (with the
Fizzy token), the state directory, and all your other files. If you need a
real boundary, run the connector as a separate OS user or in a container.
This is one more reason not to use `bypassPermissions` on your main machine.

**After a crash.** The connector records a turn as done after it ends. If
the daemon or the machine stops during a turn, the turn runs again after the
restart. A reply can then appear two times, and a command can run two times.

## Permissions

Sessions are unattended: nobody is at a terminal to answer a permission
prompt. Three settings control what a session can do.

**`permission_mode`** (override for one run: `run --permission-mode <mode>`)

| Mode | Behavior |
|---|---|
| `auto` (default) | A classifier permits usual development work and blocks risky actions. Claude gets the reason for a block and tries a different method. A permission question occurs only after repeated blocks, for the first read outside the work directories, and for "ask" rules. |
| `acceptEdits` | File edits are permitted. Each other action that is not in `allowed_tools` causes a permission question. |
| `bypassPermissions` | All actions are permitted. A mention can then run any command on the machine: use this only in a container. `doctor` prints a warning. |

**`allowed_tools`** lists actions that are always permitted, in Claude Code
permission rule syntax, for example `"Bash(go test *)"`. **`add_dirs`** lists
more directories that the sessions can read and change.

**`approvals`** (default `true`): a permission question goes to the card as a
comment. It shows the tool, its complete input, and a request code such as
`K7Q2M`. A trusted user answers with a comment that has exactly one of these:

- `yes K7Q2M` permits the action one time.
- `always K7Q2M` also permits the identical shell command in all later turns of
  that card. The connector compares the command text itself. This answer is
  available only for shell commands.
- `no K7Q2M` refuses the action.

The answer must be a new comment after the question. A mention is not
necessary in it. Other text, such as "yes" with no
code or "yes K7Q2M but wait", is not an answer. A card has one open question at
a time, and a question ends when its turn ends. If no trusted user answers
within `approval_timeout` (default 10 minutes), the action is refused and
Claude says so in its reply. A turn that waits for an answer holds one of the
`max_concurrent` slots. With `approvals = false`, each action that needs a
prompt is refused immediately.

## Tools that a session has

| Tool | Function |
|---|---|
| `reply` | Post the answer on the card of the session |
| `read_card` | Read a different card (by number or URL) and its comments |
| `search_cards` | Find cards |
| `message_card_agent` | Send a message to the session of a different card |
| `create_card` | Make a card (default: on the board of the session card) |
| `close_card`, `reopen_card` | Close or reopen a card |
| `list_columns`, `move_card` | Move a card to a column of its board |
| `list_boards`, `move_card_to_board` | Move a card to a different board |
| `assign_card` | Assign a card to a user |
| `add_tag`, `remove_tag` | Add or remove a tag on a card |
| `comment_on_card` | Comment on a different card |

The tools work on all boards that the Claude user can access. Control that
access in the board settings in Fizzy.

Thus a mention can say "implement this, and close the card when you are done",
or "research this, and open a card for each finding". The tools need no skill
and no install step: the daemon attaches them to each session.

An agent message starts a turn in the target session, in its own context. A
chain of agent messages stops after 3 hops.

## Run it

Run `fizzy-connector run` in a terminal or in a `tmux` session. The sessions
then have your shell environment: your `PATH`, your SSH agent and your other
variables. When the daemon is stopped, mentions stay as unread notifications
in Fizzy, and the daemon handles them at the next start.

## Pending work

- **A policy for old sessions.** Each turn resumes the session of its card,
  and so sends the full history of the card again. This is cheap while the
  prompt cache is warm, and expensive when it is cold. For an old card with a
  long history, a new session that starts from the card content can be the
  better choice, at the cost of some continuity. The policy is not defined
  yet. The plan is to define it from the data that `fizzy-connector sessions`
  records, for example: start a new session when the last turn is older than
  some hours and the session is larger than some number of tokens.
- **A systemd user unit** (`fizzy-connector service install`). The open point
  is the environment of the sessions: `PATH`, SSH agent and similar values.

## Tests

```sh
make build    # the binary goes to bin/
make test
make lint     # needs golangci-lint
FIZZY_CONNECTOR_REAL_CLAUDE=1 go test ./internal/daemon -run TestRealClaude
```

The last command runs one turn with the real `claude` command and your
Claude login, against a fake Fizzy server.

For manual end-to-end tests against a Fizzy development server, see
`script/e2e-lib.sh`. It has helpers to log in, make cards, mention the bot
and read the comments.
