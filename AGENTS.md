# camux — the Claude Code orchestration layer on top of amux

`camux` wraps [`amux`](../amux/AGENTS.md) with knowledge of Claude Code's
TUI. If amux is "drive a tmux pane reliably", camux is "drive Claude
reliably inside a tmux pane".

`camux` shells out to `amux` for every tmux-level operation. They're two
separate binaries; amux never depends on anything Claude-specific, and
camux can be iterated independently as Claude's TUI evolves.

## Install

```bash
git clone ... ~/dev/camux
cd ~/dev/camux && make install
# Also requires amux on PATH (see ../amux/README.md).
```

Env vars:
- `AMUX_BIN` — override the amux executable name (default `amux`).
- `CLAUDE_BIN` — override the claude executable path (default: first
  `claude` on PATH).

## Commands

| Command | What it does |
|---|---|
| `spawn <session> [--name W] [--dir D] [--no-skip-perms]` | Launch Claude in a new session, dismiss the trust dialog if it appears, block until the TUI is truly ready. Prints `session:window` on stdout. |
| `ask <target> [--timeout 180s] < prompt` | Refuse unless Ready. Paste + submit. Wait for streaming to stop. Emit the reply text (delta since submit). |
| `status <target>` | Print state: `ready` / `streaming` / `trust-dialog` / `permission-dialog` / `starting` / `not-found` / `dead`. Exit 0 if ready, 1 if busy/dialog, 2 if not-found. |
| `trust <target>` | If the trust dialog is up, confirm option 1. No-op otherwise. |
| `permit <target> [yes\|no\|always]` | Answer a tool-permission dialog. |
| `interrupt <target>` | Send Escape — cancels a streaming reply. |
| `clear <target>` | Double-Escape — clears the input buffer. |
| `slash <target> <slashcmd> [--no-enter] [--delay 80ms]` | Type `/<cmd>` char-by-char and press Enter to select. `--no-enter` leaves you in the menu for follow-up nav. |

## States

`camux status <target>` returns one of:

- **`ready`** — TUI drawn, waiting for input.
- **`streaming`** — Claude is generating a reply (status bar has ` · esc to interrupt`).
- **`trust-dialog`** — first-run "Yes, I trust this folder" prompt.
- **`permission-dialog`** — tool-use permission required (fired when not using `--dangerously-skip-permissions`).
- **`starting`** — Claude banner visible but not ready yet.
- **`not-found`** — the target doesn't exist (never existed or was killed).
- **`dead`** — reserved for future use.

Exit codes from `status`: `0` ready, `1` anything else busy, `2` not-found.

## Quickstart

```bash
TARGET=$(camux spawn build-agent)
echo "write a README.md for this repo" | camux ask $TARGET --timeout 300s
camux status $TARGET
amux kill build-agent
```

## Recipes

### Parallel fleet

```bash
for name in plan draft review; do
  camux spawn fleet-$name --dir /tmp/workspace &
done; wait

echo "draft a project plan"            | camux ask fleet-plan:cc   &
echo "start writing the introduction"  | camux ask fleet-draft:cc  &
echo "prepare review criteria"         | camux ask fleet-review:cc &
wait
```

### Defensive startup

```bash
# Works whether the session already exists or not.
if ! amux exists fleet:cc; then
  camux spawn fleet
fi
# And whether the trust dialog shows up or not.
camux trust fleet:cc
echo "hi" | camux ask fleet:cc
```

### Interrupt a runaway reply

```bash
echo "write 10,000 haikus" | camux ask runaway:cc --timeout 5s
# ↑ times out; Claude is still streaming
camux interrupt runaway:cc
camux status runaway:cc   # → ready
```

### Persistent transcripts

camux itself doesn't log; use amux's `log` primitive:

```bash
camux spawn agent >/dev/null
amux log agent:cc /tmp/agent.transcript
echo "task..." | camux ask agent:cc
# /tmp/agent.transcript now has everything the pane displayed.
```

## Gotchas

### 1. `ask` refuses unless Ready

By design. If the target is streaming, in a dialog, or starting, you
need to resolve that first. The error message tells you which command
to use:

```
$ echo "hi" | camux ask sess:cc
camux: ask: sess:cc is in state "permission-dialog", not ready.
Handle with camux permit first.
```

### 2. The "esc to interrupt" signal

Ready vs. streaming is detected by the presence of ` · esc to interrupt`
in Claude's status bar. If Anthropic changes that exact text, camux's
state detection breaks. If you see `ask` hang past your timeout while
Claude is clearly done, first check that signal with:

```
amux capture sess:cc | grep 'esc to interrupt'
```

### 3. `trust` only works with the default choice

The trust dialog's default is "Yes, I trust this folder" — `camux trust`
sends Enter. If you want to refuse trust, don't — just don't spawn
Claude in that directory. Run it somewhere you trust.

### 4. `permit` is best-effort on complex dialogs

Permission dialogs vary. `permit yes/no/always` assumes the top three
options in that order. For complex multi-choice dialogs, use the raw
amux primitives:

```bash
amux key sess:cc Down --repeat 3 --delay 80ms
amux key sess:cc Enter
```

### 5. Keyboard power moves (via `amux key`)

Claude's built-in TUI shortcuts (gleaned from the published internals):

| Key | What |
|---|---|
| `C-c` | Interrupt (double-tap to quit) |
| `C-d` | Exit session (double-tap to confirm) |
| `C-l` | Redraw screen |
| `C-t` | Toggle todos panel |
| `C-o` | Toggle verbose/transcript view |
| `C-r` | Search history |
| `C-s` | Stash the current draft |
| `C-g` | Open input in external editor |
| `S-Tab` | Cycle permission mode (normal ↔ bypass ↔ auto-accept) |
| `M-p` | Model picker |
| `M-o` | Fast mode toggle |
| `M-t` | Thinking toggle |
| `Up` / `Down` | Scroll input history |
| `Escape` | Cancel / interrupt (single tap in chat) |
| `Escape Escape` | Clear input buffer |

All of these are sendable via `amux key sess:cc <KeyName>`. camux's
`interrupt` / `clear` / `slash` just bundle the common ones.

## How it composes with amux

Every camux command is a thin state-machine + amux invocation:

- `spawn` = `amux new` + `amux window ...claude...` + poll `status` until
  ready, dismissing the trust dialog en route.
- `ask` = assert state is ready + snapshot `history_size+cursor_y` via
  tmux + `amux paste --submit` + wait for `esc to interrupt` to appear
  then disappear + emit the delta via tmux `capture-pane -S`.
- `status` = `amux exists` + `amux capture` + regex classify.
- `trust` / `permit` / `interrupt` / `clear` = targeted `amux key`.
- `slash` = `amux type --delay` + pause + `amux key Enter`.

You can always go one layer down and use amux directly. camux is the
convenient path; amux is the escape hatch.
