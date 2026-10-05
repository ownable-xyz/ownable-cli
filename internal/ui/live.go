// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ui

import (
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// View is what a live region shows, read on every redraw: the bar's fill
// (negative when the total is unknown), the headline beside the bar, and a
// line of detail under it. It is called from the redraw goroutine, so it
// must be safe for concurrent use with whatever updates its state.
type View func() (frac float64, head, detail string)

// Live is a two-line region that redraws in place: the bar row and the
// detail row. On a terminal it redraws ten times a second, so clocks and
// countdowns move between updates; elsewhere it prints a plain progress
// line every 30 seconds, for logs.
//
// Each redraw is one write that overwrites the region line by line, so the
// terminal never shows it blank; a frame equal to the last one is not
// written at all; and the write is wrapped in synchronized-update mode
// (DEC 2026), which terminals that know it paint atomically and the rest
// ignore.
type Live struct {
	t     *Term
	label string
	view  View
	spin  int
	drawn int    // lines of the region currently on screen
	last  string // the frame on screen
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

const (
	syncOn  = "\x1b[?2026h"
	syncOff = "\x1b[?2026l"
)

// Live opens a live region labelled label.
func (t *Term) Live(label string, view View) *Live {
	l := &Live{t: t, label: label, view: view, stop: make(chan struct{}), done: make(chan struct{})}
	if t.quiet {
		close(l.done)
		return l
	}
	t.closeLive(false) // one region at a time
	t.mu.Lock()
	t.live = l
	if t.tty {
		io.WriteString(t.w, "\x1b[?25l") // hide the cursor while redrawing
		l.redrawLocked("")
	}
	t.mu.Unlock()
	go l.loop()
	return l
}

func (l *Live) loop() {
	defer close(l.done)
	every := 30 * time.Second
	if l.t.tty {
		every = 100 * time.Millisecond
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-tick.C:
			l.t.mu.Lock()
			if l.t.tty {
				l.spin++
				l.redrawLocked("")
			} else {
				_, head, detail := l.view()
				io.WriteString(l.t.w, l.t.gutter(l.label)+strings.TrimSpace(head+"  "+detail)+"\n")
			}
			l.t.mu.Unlock()
		}
	}
}

// frame renders the region's lines, each cut to the terminal's width.
func (l *Live) frame() []string {
	frac, head, detail := l.view()
	w := l.t.width() - 1
	rows := []string{l.t.gutter(l.label) + l.t.bar(frac, l.spin) + "  " + head}
	if detail != "" {
		rows = append(rows, "  "+strings.Repeat(" ", Gutter)+l.t.Label(detail))
	}
	for i, r := range rows {
		rows[i] = truncate(r, w)
	}
	return rows
}

// up moves the cursor to the start of the region's first line.
func (l *Live) up() string {
	if l.drawn == 0 {
		return "\r"
	}
	return "\x1b[" + strconv.Itoa(l.drawn) + "A\r"
}

// redrawLocked writes above (rows printed while the region is open, which
// take its place) and then the region below them, in one write. With
// nothing above and an unchanged frame it writes nothing.
func (l *Live) redrawLocked(above string) {
	rows := l.frame()
	f := strings.Join(rows, "\n")
	if above == "" && f == l.last && l.drawn > 0 {
		return
	}
	var b strings.Builder
	b.WriteString(syncOn)
	b.WriteString(l.up())
	if above != "" {
		b.WriteString("\x1b[0J") // the rows above are printed whole
		b.WriteString(above)
	}
	for _, r := range rows {
		b.WriteString(r)
		b.WriteString("\x1b[K\n") // clear what the last frame left beyond this one
	}
	if above == "" && len(rows) < l.drawn {
		b.WriteString("\x1b[0J")
	}
	b.WriteString(syncOff)
	io.WriteString(l.t.w, b.String())
	l.drawn, l.last = len(rows), f
}

// Close ends the region and leaves its final form: the headline with a
// mark (a full bar on a terminal when ok) and the detail line. Closing
// twice is harmless.
func (l *Live) Close(head, detail string, ok bool) {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		t := l.t
		if t.quiet {
			return
		}
		row := t.gutter(l.label)
		if t.tty && ok {
			row += t.bar(1, 0) + "  "
		}
		final := row + head + " " + t.mark(ok) + "\n"
		if detail != "" {
			final += "  " + strings.Repeat(" ", Gutter) + t.Label(detail) + "\n"
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.live == l {
			t.live = nil
		}
		if !t.tty {
			io.WriteString(t.w, final)
			return
		}
		// The final rows replace the region in the same write.
		io.WriteString(t.w, syncOn+l.up()+"\x1b[0J"+final+"\x1b[?25h"+syncOff)
		l.drawn = 0
	})
}
