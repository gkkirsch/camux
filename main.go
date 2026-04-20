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

camux delegates to the amux binary for all tmux-level operations. Set
AMUX_BIN to override the amux executable name, CLAUDE_BIN to override the
claude executable path.
`

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
		fmt.Print(usageText)
		return
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
