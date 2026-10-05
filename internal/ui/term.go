// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package ui is ownable's terminal output: a header, a column of labelled
// rows, and a live region that redraws in place while a long phase runs.
//
// Color and motion are used only on an interactive terminal. Anything else
// (a pipe, a file) gets the same rows as plain text and no live region;
// NO_COLOR and --no-color keep the layout and drop the color. Primary text
// keeps the terminal's own foreground so the output reads on light and dark
// backgrounds alike; color marks structure (slate labels), progress (the
// violet bar) and verdicts (green, red, amber).
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Palette. Foregrounds only; the terminal's background is never set.
const (
	cLabel = "138;148;158" // #8A949E slate: labels, notes, rules
	cDim   = "88;96;105"   // #586069: the unfilled part of the bar
	cOK    = "95;184;120"  // #5FB878: success, identical
	cBad   = "229;83;75"   // #E5534B: failure, mismatch
	cWarn  = "217;164;65"  // #D9A441: weaker statements (fork mode)
)

// The bar's fill runs from indigo to periwinkle (#6E6AF0 to #A9A6FF).
var barLo, barHi = [3]int{0x6E, 0x6A, 0xF0}, [3]int{0xA9, 0xA6, 0xFF}

// Gutter is the width of the label column.
const Gutter = 13

// barCells is the width of the progress bar.
const barCells = 24

// Term writes to one stream (stdout or stderr). It is safe for concurrent
// use: rows printed while a live region is open appear above it.
type Term struct {
	mu    sync.Mutex
	f     *os.File
	w     io.Writer
	tty   bool
	color bool
	quiet bool
	live  *Live
}

// New returns a Term for f. quiet suppresses the transcript (Header, Row,
// Phase, Note, Live); Field and Fail always print.
func New(f *os.File, quiet, noColor bool) *Term {
	t := &Term{f: f, w: f, quiet: quiet}
	if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		t.tty = enableVT(f)
	}
	t.color = t.tty && !noColor && os.Getenv("NO_COLOR") == ""
	return t
}

// TTY reports whether the Term is an interactive terminal.
func (t *Term) TTY() bool { return t.tty }

// Shows reports whether transcript rows are printed (not --quiet).
func (t *Term) Shows() bool { return !t.quiet }

func (t *Term) width() int {
	if w := termWidth(t.f); w > 20 {
		return w
	}
	return 80
}

func (t *Term) paint(rgb, s string) string {
	if !t.color || s == "" {
		return s
	}
	return "\x1b[38;2;" + rgb + "m" + s + "\x1b[0m"
}

// Label, OK, Bad and Warn color s for the role they name.
func (t *Term) Label(s string) string { return t.paint(cLabel, s) }
func (t *Term) OK(s string) string    { return t.paint(cOK, s) }
func (t *Term) Bad(s string) string   { return t.paint(cBad, s) }
func (t *Term) Warn(s string) string  { return t.paint(cWarn, s) }

func (t *Term) bold(s string) string {
	if !t.color {
		return s
	}
	return "\x1b[1m" + s + "\x1b[22m"
}

func (t *Term) accent(frac float64, s string) string {
	if !t.color {
		return s
	}
	c := [3]int{}
	for i := range c {
		c[i] = barLo[i] + int(frac*float64(barHi[i]-barLo[i]))
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", c[0], c[1], c[2], s)
}

// bar draws the progress bar: frac in [0,1] fills it; frac < 0 means the
// total is unknown, and a short glow travels along it instead.
func (t *Term) bar(frac float64, spin int) string {
	var b strings.Builder
	if frac < 0 {
		pos := spin % (barCells + 3)
		for i := 0; i < barCells; i++ {
			switch pos - i {
			case 0:
				b.WriteString(t.accent(1, "■"))
			case 1, 2:
				b.WriteString(t.accent(0.3, "■"))
			default:
				b.WriteString(t.paint(cDim, "□"))
			}
		}
		return b.String()
	}
	fill := int(frac*barCells + 0.5)
	fill = min(max(fill, 0), barCells)
	for i := 0; i < fill; i++ {
		b.WriteString(t.accent(float64(i)/float64(barCells-1), "■"))
	}
	b.WriteString(t.paint(cDim, strings.Repeat("□", barCells-fill)))
	return b.String()
}

func (t *Term) gutter(label string) string {
	return "  " + t.Label(pad(label, Gutter))
}

func pad(s string, n int) string {
	if k := n - visible(s); k > 0 {
		return s + strings.Repeat(" ", k)
	}
	return s + " "
}

// emit writes s, keeping an open live region below it.
func (t *Term) emit(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l := t.live; l != nil && t.tty {
		l.redrawLocked(s)
		return
	}
	io.WriteString(t.w, s)
}

// Header prints the wordmark and a rule.
func (t *Term) Header(version, tagline string) {
	if t.quiet || !t.tty {
		return
	}
	const measure = 68
	right := version + " · " + tagline
	gap := max(2, measure-len("o w n a b l e")-visible(right))
	rule := min(measure, t.width()-4)
	t.emit("\n  " + t.bold("o w n a b l e") + strings.Repeat(" ", gap) + t.Label(right) + "\n" +
		"  " + t.Label(strings.Repeat("─", rule)) + "\n")
}

// Row prints a labelled transcript row.
func (t *Term) Row(label, text string) {
	if t.quiet {
		return
	}
	t.emit(t.gutter(label) + text + "\n")
}

// Phase prints a finished step with its mark.
func (t *Term) Phase(label, text string, ok bool) {
	if t.quiet {
		return
	}
	t.emit(t.gutter(label) + text + " " + t.mark(ok) + "\n")
}

func (t *Term) mark(ok bool) string {
	if ok {
		return t.OK("✓")
	}
	return t.Bad("✗")
}

// Note prints a detail line under the previous row.
func (t *Term) Note(text string) {
	if t.quiet {
		return
	}
	t.emit("  " + strings.Repeat(" ", Gutter) + t.Label(text) + "\n")
}

// Blank prints an empty transcript line on a terminal.
func (t *Term) Blank() {
	if t.quiet || !t.tty {
		return
	}
	t.emit("\n")
}

// Rule prints a thin rule on a terminal.
func (t *Term) Rule() {
	if !t.tty {
		return
	}
	t.emit("  " + t.Label(strings.Repeat("─", min(68, t.width()-4))) + "\n")
}

// Field prints a result row. On a terminal it is a labelled row; elsewhere
// it is the plain "label  value" line, the label padded to plainW columns,
// which is what scripts read. An empty label continues the previous field.
func (t *Term) Field(label, value string, plainW int) {
	if t.tty {
		t.emit(t.gutter(label) + value + "\n")
		return
	}
	t.emit(fmt.Sprintf("%-*s %s\n", plainW, label, value))
}

// Stamp prints the closing line on a terminal: the verdict in one sentence.
func (t *Term) Stamp(ok bool, text string) {
	if !t.tty {
		return
	}
	if ok {
		t.emit("\n  " + t.mark(true) + " " + t.OK(text) + "\n\n")
	} else {
		t.emit("\n  " + t.mark(false) + " " + t.Bad(text) + "\n\n")
	}
}

// Fail prints an error: on a terminal a red line and an optional hint,
// elsewhere "ownable: <error>".
func (t *Term) Fail(err error, hint string) {
	t.closeLive(false)
	if !t.tty {
		t.emit("ownable: " + err.Error() + "\n")
		if hint != "" {
			t.emit("ownable: " + hint + "\n")
		}
		return
	}
	t.emit("\n  " + t.Bad("✗ "+err.Error()) + "\n")
	if hint != "" {
		t.emit("    " + t.Label(hint) + "\n")
	}
	t.emit("\n")
}

// Close ends an open live region (as failed) and restores the cursor.
func (t *Term) Close() { t.closeLive(false) }

func (t *Term) closeLive(ok bool) {
	t.mu.Lock()
	l := t.live
	t.mu.Unlock()
	if l != nil {
		l.Close("interrupted", "", ok)
	}
}

// visible is the number of terminal columns s takes: runes, less escapes.
func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

// truncate cuts s to at most w visible columns, keeping its escapes, and
// marks the cut with an ellipsis.
func truncate(s string, w int) string {
	if visible(s) <= w {
		return s
	}
	var b strings.Builder
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			b.WriteRune(r)
			if r >= '@' && r <= '~' && r != '[' {
				esc = false
			}
		case r == '\x1b':
			esc = true
			b.WriteRune(r)
		default:
			if n < w-1 {
				b.WriteRune(r)
			} else if n == w-1 {
				b.WriteString("…")
			}
			n++
		}
	}
	b.WriteString("\x1b[0m")
	return b.String()
}
