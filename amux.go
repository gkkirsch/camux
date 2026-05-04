package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// amuxBinName is the amux executable we delegate to. Resolution order:
//
//  1. $AMUX_BIN if set (explicit override, wins everything).
//  2. The `amux` binary sitting next to this `camux` binary on disk.
//     We bundle them together in Director.app/Contents/MacOS, in
//     roster's release tarball, and in any sane local install. When
//     the binaries ship together they should behave together — PATH
//     order on the user's machine should not be able to wedge in a
//     different `amux` between us. (That happened in the wild on a
//     friend's install, where an unrelated tool named amux on his
//     PATH was getting picked up ahead of our bundled one and
//     failing the existence check, producing the runaway-tmux bug.)
//  3. PATH lookup, as a last resort.
var amuxBinName = "amux"

// resolveAmuxBin runs the sibling lookup. Called once from main()
// before the env override so that env still wins.
func resolveAmuxBin() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return
	}
	sib := filepath.Join(filepath.Dir(real), "amux")
	fi, err := os.Stat(sib)
	if err != nil || fi.IsDir() {
		return
	}
	if fi.Mode()&0o111 == 0 {
		return
	}
	amuxBinName = sib
}

// runAmux calls `amux <args...>` and returns stdout. Non-zero exit becomes
// an error whose message embeds stderr.
func runAmux(args ...string) (string, error) {
	return runAmuxStdin(nil, args...)
}

func runAmuxStdin(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command(amuxBinName, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("amux %s: %s", strings.Join(args, " "), msg)
	}
	return out.String(), nil
}

// amuxExists returns true iff the amux `exists` check succeeds on target.
// Silent, no stderr on success; non-zero exit = does not exist.
func amuxExists(target string) bool {
	cmd := exec.Command(amuxBinName, "exists", target)
	return cmd.Run() == nil
}

// capture is a convenience around `amux capture`.
func capture(target string, lines int) (string, error) {
	args := []string{"capture", target}
	if lines > 0 {
		args = append(args, "--lines", fmt.Sprint(lines))
	}
	return runAmux(args...)
}
