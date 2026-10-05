// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ui

import "testing"

func TestVisibleAndTruncate(t *testing.T) {
	s := "\x1b[38;2;1;2;3mabc\x1b[0m■□def"
	if n := visible(s); n != 8 {
		t.Fatalf("visible = %d, want 8", n)
	}
	if got := truncate(s, 8); got != s {
		t.Fatalf("fits: got %q", got)
	}
	got := truncate(s, 5)
	if n := visible(got); n != 5 {
		t.Fatalf("truncated width = %d, want 5 (%q)", n, got)
	}
	if want := "\x1b[38;2;1;2;3mabc\x1b[0m■…\x1b[0m"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBarWidth(t *testing.T) {
	tm := &Term{}
	for _, f := range []float64{-1, 0, 0.37, 1, 2} {
		if n := visible(tm.bar(f, 7)); n != barCells {
			t.Fatalf("bar(%v) is %d cells, want %d", f, n, barCells)
		}
	}
}
