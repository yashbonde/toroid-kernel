//go:build darwin || linux
// +build darwin linux

package main

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"
)

type cbreakState struct {
	termios *unix.Termios
}

func enableCBreak(fd int) (*cbreakState, error) {
	t, err := unix.IoctlGetTermios(fd, getTermios())
	if err != nil {
		return nil, err
	}
	old := *t
	t.Lflag &^= unix.ICANON | unix.ECHO
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, setTermios(), t); err != nil {
		return nil, err
	}
	return &cbreakState{termios: &old}, nil
}

func restoreCBreak(fd int, s *cbreakState) error {
	if s == nil || s.termios == nil {
		return nil
	}
	return unix.IoctlSetTermios(fd, setTermios(), s.termios)
}

// watchEsc polls stdin for the ESC key while ctx is live, cancelling ctx and
// returning as soon as it sees one. It always closes done on exit so the
// caller can wait for the read loop to actually stop touching fd before
// handing stdin back to the normal line-buffered reader.
func watchEsc(ctx context.Context, fd int, cancel func(), done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		rfds := &unix.FdSet{}
		rfds.Set(fd)
		tv := &unix.Timeval{Sec: 0, Usec: 100000} // 100 ms

		n, err := unix.Select(fd+1, rfds, nil, nil, tv)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n <= 0 {
			continue
		}

		buf := make([]byte, 1)
		nr, err := unix.Read(fd, buf)
		if err != nil || nr != 1 {
			continue
		}
		if buf[0] == 27 { // ESC
			fmt.Printf("\n%s[interrupted]%s\n", aYellow, aReset)
			cancel()
			return
		}
	}
}
