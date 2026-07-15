//go:build !(darwin || linux)
// +build !darwin,!linux

package main

import "context"

type cbreakState struct{}

func enableCBreak(fd int) (*cbreakState, error) {
	return nil, nil
}

func restoreCBreak(fd int, s *cbreakState) error {
	return nil
}

func watchEsc(ctx context.Context, fd int, cancel func(), done chan<- struct{}) {
	close(done)
}
