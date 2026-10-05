// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ui

import (
	"bytes"
	"strings"
	"testing"
)

// A redraw is one write, never blanks the region before repainting it, and
// an unchanged frame writes nothing.
func TestRedrawWritesOnceAndOnlyChanges(t *testing.T) {
	var out countingWriter
	tm := &Term{w: &out, tty: true}
	head := "10%"
	l := &Live{t: tm, label: "stream", view: func() (float64, string, string) { return 0.1, head, "detail" }}

	l.redrawLocked("")
	if out.writes != 1 || l.drawn != 2 {
		t.Fatalf("first frame: %d writes, %d lines drawn", out.writes, l.drawn)
	}
	l.redrawLocked("")
	if out.writes != 1 {
		t.Fatal("an unchanged frame was written again")
	}
	head = "11%"
	out.Reset()
	l.redrawLocked("")
	got := out.String()
	if out.writes != 2 || !strings.HasPrefix(got, syncOn+"\x1b[2A\r") || strings.Contains(got, "\x1b[0J") || !strings.Contains(got, "11%") {
		t.Fatalf("changed frame: %d writes, %q", out.writes, got)
	}
	// A row printed while the region is open goes above it, in the same write.
	out.Reset()
	l.redrawLocked("  noise    done\n")
	if got := out.String(); out.writes != 3 || strings.Index(got, "noise") > strings.Index(got, "11%") {
		t.Fatalf("row above: %d writes, %q", out.writes, got)
	}
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func (w *countingWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
