package main

import (
	"fmt"
	"os"
)

const usageText = `camux — Claude-Code-aware orchestration layer on top of amux.

Usage:
  camux <command> [flags] [args]

Commands:
  spawn  <session> [--name W] [--dir D] [--no-skip-perms]
         Launch Claude Code in a new amux session, dismiss the trust dialog
         if it appears, and block until the TUI is truly ready. Prints the
         target (session:window) on success.

  ask    <target> [--timeout 180s] < prompt
         Refuse unless the target is Ready. Submit the prompt via amux paste
         --submit. Wait for the streaming status to clear. Emit the reply
         text (delta since submit).

  status <target>
         Print one of: ready, streaming, trust-dialog, permission-dialog,
         starting, not-found, dead. Exit 0 if ready, 1 if busy/dialog,
         2 if not-found.

  trust  <target>
         If the trust-folder dialog is showing, confirm option 1 and
         block until we're past it. No-op when not in trust state.

  permit <target> [yes|no|always]
         Answer a tool-permission dialog. Default is "yes". The three
         choices correspond to the top three options in Claude's
         permission UI.

  interrupt <target>
         Single Escape — stops a streaming reply or dismisses an overlay.

  clear <target>
         Double Escape — Claude's shortcut for clearing the input buffer.

  slash <target> <slashcmd> [--no-enter] [--delay 80ms]
         Type "/<slashcmd>" char-by-char (so rich-TUI search filters see
         each keystroke) and press Enter to select. --no-enter leaves
         the cursor in the menu for follow-up navigation.

  model <target> <model>
         Switch model in-session via /model <name>. e.g. sonnet, opus,
         haiku-4-5, or a full model ID.

  plugin <subcmd> [args...]
         Thin wrapper around 'claude plugin' — install, uninstall, list,
         enable, disable, update, marketplace.

  auth [status|login|logout]
         Thin wrapper around 'claude auth'. Passes stdin/stdout through
         so login can be completed interactively.

  sessions [--json] [--all]
         List all amux panes that look like Claude processes, with each
         pane's state (ready/streaming/dialog/...). --all includes
         non-Claude panes.

Spawn flags (camux spawn <sess> --flag value):
  --model M                  --system-prompt "..."   --append-system "..."
  --effort LEVEL             --permission-mode MODE  --display-name "..."
  --session-id UUID          --resume ID             --continue
  --agents JSON              --add-dir PATH
  --no-skip-perms            --timeout DUR

camux delegates to the amux binary for all tmux-level operations. Set
AMUX_BIN to override the amux executable name, CLAUDE_BIN to override the
claude executable path.
`

// commandHelp maps subcommand → long help text. `camux <cmd> -h` and
// `camux help <cmd>` both print it.
var commandHelp = map[string]string{
	"spawn": `camux spawn <session> [flags]

Launch Claude Code in a new amux session, handle the trust-folder dialog
if it appears, and block until the TUI is truly ready. Prints the target
(session:window) on stdout.

Flags (all but --name/--dir/--timeout/--no-skip-perms pass through to
the 'claude' CLI):
  --name W                Window name inside the session. Default "cc".
  --dir PATH              Launch Claude with --add-dir PATH (that dir
                          becomes Claude's cwd).
  --no-skip-perms         Don't add --dangerously-skip-permissions.
  --timeout DUR           Wait up to DUR for the TUI to become ready.
                          Default 60s.
  --model M               claude --model  (sonnet, opus, haiku-4-5, or
                          full model ID).
  --system-prompt "..."   claude --system-prompt (replaces default).
  --append-system "..."   claude --append-system-prompt (appends).
  --effort LEVEL          low|medium|high|xhigh|max.
  --permission-mode M     acceptEdits|auto|bypassPermissions|default|
                          dontAsk|plan.
  --display-name "..."    Name shown in Claude's prompt box.
  --session-id UUID       Pin a specific Claude session UUID.
  --resume ID             Resume a past conversation by session ID.
  --continue              Continue the most recent conversation in cwd.
  --agents JSON           JSON object defining custom agents.

Examples:
  camux spawn work
  camux spawn planner --model sonnet --effort high
  camux spawn fix --resume $(camux info old:cc --json | jq -r .session_id)`,

	"ask": `camux ask <target> [flags] < prompt

Refuse unless <target> is Ready. Paste the prompt (bracketed), submit
via Enter, wait for Claude to finish streaming, and print the reply
delta (new content added since submit).

Flags:
  --timeout DUR           Overall wait. Default 180s.
  --interval DUR          Poll interval. Default 400ms.
  --auto-permit MODE      Auto-answer permission dialogs mid-reply with
                          yes|no|always. Default: bail with error so
                          the orchestrator can decide.
  --auto-trust            Auto-dismiss trust dialogs mid-reply.

On a permission dialog with no --auto-permit, ask exits 1 with a message
that tells you which camux command to run next (usually 'camux permit
<target> yes' then 'camux wait <target>').

Examples:
  echo "what is 17*23?" | camux ask sess:cc
  cat prompt.md | camux ask sess:cc --auto-permit always --timeout 300s`,

	"status": `camux status <target>

Print one of: ready, streaming, trust-dialog, permission-dialog,
starting, not-found, dead. Exit codes: 0 ready, 1 busy/dialog,
2 not-found.`,

	"sessions": `camux sessions [--json] [--all]

List every amux pane that looks like a Claude process, with per-pane
state. --all includes non-Claude panes. --json emits structured output.`,

	"info": `camux info <target> [--json]

Run /status inside the TUI, parse the result, print it. Fields:
version, session_name, session_id (UUID), cwd, login_method,
organization, email, model, mcp_servers.

Use --json to pipe into jq. The session_id field enables
'camux spawn <sess> --resume $id' to rehydrate the conversation.`,

	"trust":     `camux trust <target>  —  confirm the trust-folder dialog (Enter on option 1). No-op if not in that state.`,
	"permit":    `camux permit <target> [yes|no|always]  —  answer a tool-permission dialog.`,
	"interrupt": `camux interrupt <target>  —  Escape, stops streaming / dismisses overlay.`,
	"clear":     `camux clear <target>  —  double-Escape, clears input buffer (Claude's shortcut).`,
	"slash": `camux slash <target> <slashcmd> [--no-enter] [--delay 80ms]

Type /<slashcmd> char-by-char (so picker search fields see each
keystroke), optionally press Enter to select. --no-enter leaves the
menu open for follow-up navigation via 'amux key'.`,
	"model":  `camux model <target> <model>  —  /model <name> in-session.`,
	"reload": `camux reload <target>  —  /reload-plugins in-session; prints the summary line.`,
	"wait": `camux wait <target> [flags]

Block until <target> is Ready, automatically resolving trust/permission
dialogs along the way. Use after 'ask' bailed on a dialog.

Flags:
  --timeout DUR           Default 180s.
  --interval DUR          Default 400ms.
  --auto-permit MODE      yes|no|always|off. Default yes.
  --auto-trust            Default true.`,
	"plugin": `camux plugin <subcmd> [args...]

Thin wrapper over 'claude plugin':
  list              List installed plugins.
  install <name>    Install from marketplaces (name@marketplace).
  uninstall <name>
  enable <name> / disable <name>
  update <name>
  marketplace <add|list|remove|update>   Manage sources.

Plugins installed while a Claude session is running don't take effect
until you run 'camux reload <target>'.`,
	"auth": `camux auth [status|login|logout]

Wraps 'claude auth'. Passes stdin/stdout/stderr so interactive login
flows work. Use 'auth status' non-interactively to check login state.`,
}

func main() {
	if v := os.Getenv("AMUX_BIN"); v != "" {
		amuxBinName = v
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		if len(args) == 0 {
			fmt.Print(usageText)
			return
		}
		if h, ok := commandHelp[args[0]]; ok {
			fmt.Println(h)
			return
		}
		fmt.Fprintf(os.Stderr, "camux: no help for %q\n", args[0])
		os.Exit(2)
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		if h, ok := commandHelp[cmd]; ok {
			fmt.Println(h)
			return
		}
	}
	var err error
	switch cmd {
	case "spawn":
		err = cmdSpawn(args)
	case "ask":
		err = cmdAsk(args)
	case "status":
		err = cmdStatus(args)
	case "trust":
		err = cmdTrust(args)
	case "permit":
		err = cmdPermit(args)
	case "interrupt":
		err = cmdInterrupt(args)
	case "clear":
		err = cmdClear(args)
	case "slash":
		err = cmdSlash(args)
	case "plugin", "plugins":
		err = cmdPlugin(args)
	case "auth":
		err = cmdAuth(args)
	case "sessions", "ls":
		err = cmdSessions(args)
	case "model":
		err = cmdModel(args)
	case "reload":
		err = cmdReload(args)
	case "wait":
		err = cmdWait(args)
	case "info":
		err = cmdInfo(args)
	default:
		fmt.Fprintf(os.Stderr, "camux: unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "camux:", err)
		os.Exit(1)
	}
}
