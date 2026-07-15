//go:build darwin
// +build darwin

package main

import "golang.org/x/sys/unix"

func getTermios() uint { return unix.TIOCGETA }
func setTermios() uint { return unix.TIOCSETA }
