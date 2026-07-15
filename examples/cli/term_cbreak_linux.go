//go:build linux
// +build linux

package main

import "golang.org/x/sys/unix"

func getTermios() uint { return unix.TCGETS }
func setTermios() uint { return unix.TCSETS }
