//go:build darwin || linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	"github.com/creack/pty"
	toroid "github.com/yashbonde/toroid-kernel"
	"golang.org/x/sys/unix"
)

// A '!' command must never touch the terminal the TUI holds in raw mode. The
// regression this guards: bashSubmit used to run `zsh -i`, an interactive shell
// that (a) sources ~/.zshrc itself and blocks on any /dev/tty prompt in it
// (compinit's "Ignore insecure directories?", prompt frameworks) and (b) takes
// over the tty for job control, freezing the whole UI rather than just the
// command.
//
// Reproducing that needs a real controlling terminal, so the test allocates a
// pty and re-execs itself as a session leader attached to it. The child runs
// bashSubmit against a HOME whose .zshrc hangs forever when interactive, then
// reports whether raw mode survived.

const (
	ptyChildEnv  = "TOROID_PTY_CHILD"
	ptyChildDone = "PTY_CHILD_DONE"
)

func TestBashSubmitDoesNotTouchTheTerminal(t *testing.T) {
	if os.Getenv(ptyChildEnv) == "1" {
		ptyChild(t)
		return
	}
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh not available")
	}

	// A HOME whose rc blocks forever under an interactive shell, and defines an
	// alias unconditionally so we can prove alias expansion still works.
	home := t.TempDir()
	rc := "if [[ -o interactive ]]; then read -k 1 x </dev/tty; fi\nalias gst='echo ALIAS_OK'\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=TestBashSubmitDoesNotTouchTheTerminal", "-test.v")
	child.Env = append(os.Environ(), ptyChildEnv+"=1", "HOME="+home, "ZDOTDIR="+home)

	// Session leader with the pty as its controlling terminal — exactly the
	// situation the real TUI runs in.
	ptmx, err := pty.Start(child)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	defer func() { _ = ptmx.Close() }()

	out := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, ptmx) // ends at child exit (EIO on the master)
		out <- buf.String()
	}()

	waited := make(chan error, 1)
	go func() { waited <- child.Wait() }()

	select {
	case err := <-waited:
		got := <-out
		if err != nil {
			t.Fatalf("child failed: %v\n%s", err, got)
		}
		if !strings.Contains(got, ptyChildDone) {
			t.Fatalf("child never completed the command:\n%s", got)
		}
		if !strings.Contains(got, "ALIAS_OK") {
			t.Fatalf("alias from ~/.zshrc did not expand (eval-after-source broken):\n%s", got)
		}
		if strings.Contains(got, "RAW_MODE_CLOBBERED") {
			t.Fatalf("the '!' command changed the TUI's terminal modes:\n%s", got)
		}
	case <-time.After(20 * time.Second):
		// The old `zsh -i` path lands here: blocked on /dev/tty forever.
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		t.Fatalf("bashSubmit hung with a controlling terminal present:\n%s", <-out)
	}
}

// ptyChild runs inside the pty as the TUI would: raw mode on, then one '!'
// command through bashSubmit.
func ptyChild(t *testing.T) {
	fd := int(os.Stdin.Fd())
	st, err := enableCBreak(fd)
	if err != nil {
		fmt.Println("cbreak:", err)
		return
	}
	before, _ := unix.IoctlGetTermios(fd, getTermios())

	input := textarea.New()
	transcript := viewport.New()
	m := &tuiModel{
		width: 80, height: 24, input: input, transcript: transcript,
		cfg:    &config{model: "test", workdir: t.TempDir()},
		kernel: &toroid.Kernel{Cfg: toroid.Config{TotalContextSize: 200000}},
	}
	m.resize()

	msg := m.bashSubmit("!gst")().(tuiBashDoneMsg)

	after, _ := unix.IoctlGetTermios(fd, getTermios())
	if before != nil && after != nil && (before.Lflag != after.Lflag || before.Iflag != after.Iflag) {
		fmt.Println("RAW_MODE_CLOBBERED")
	}
	_ = restoreCBreak(fd, st)

	fmt.Printf("%s\n%s out=%q elapsed=%s\n", msg.out, ptyChildDone, msg.out, msg.elapsed)
}
