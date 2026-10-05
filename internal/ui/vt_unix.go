// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

//go:build unix

package ui

import (
	"os"

	"golang.org/x/sys/unix"
)

// enableVT: Unix terminals speak ANSI natively.
func enableVT(*os.File) bool { return true }

// termWidth is the terminal's width in columns, or 0.
func termWidth(f *os.File) int {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}
