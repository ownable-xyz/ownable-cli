// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package main

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/history"
	"github.com/ownable-xyz/ownable-cli/internal/stamp"
	"github.com/ownable-xyz/ownable-cli/internal/stepkvm"
)

// rate measures how fast a counter grows, over a sliding window, so that a
// slow start (the interpreter's step budget ramps up over its first calls)
// does not drag the estimate down for the rest of the run.
type rate struct {
	window  time.Duration
	samples []sample
}

type sample struct {
	at time.Time
	v  float64
}

func (r *rate) add(at time.Time, v float64) {
	r.samples = append(r.samples, sample{at, v})
	// Keep one sample older than the window, so the window stays full.
	for len(r.samples) > 2 && at.Sub(r.samples[1].at) > r.window {
		r.samples = r.samples[1:]
	}
}

// perSecond is the rate over the window, or 0 until it is meaningful: at
// least three samples spanning five seconds.
func (r *rate) perSecond() float64 {
	if len(r.samples) < 3 {
		return 0
	}
	a, b := r.samples[0], r.samples[len(r.samples)-1]
	dt := b.at.Sub(a.at).Seconds()
	if dt < 5 || b.v <= a.v {
		return 0
	}
	return (b.v - a.v) / dt
}

// countdown turns a remaining amount and a rate into a finish time that
// moves smoothly: each new estimate is blended into the last, and between
// updates the time left simply runs down.
type countdown struct {
	finish time.Time
}

func (c *countdown) update(now time.Time, remaining, perSecond float64) {
	if perSecond <= 0 {
		return
	}
	next := now.Add(time.Duration(remaining / perSecond * float64(time.Second)))
	if c.finish.IsZero() {
		c.finish = next
		return
	}
	c.finish = c.finish.Add(time.Duration(0.25 * float64(next.Sub(c.finish))))
}

func (c *countdown) left(now time.Time) (time.Duration, bool) {
	if c.finish.IsZero() {
		return 0, false
	}
	return max(c.finish.Sub(now), 0), true
}

// leftText is a time remaining, rounded to what a person can use.
func leftText(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return "a few seconds left"
	case d < time.Minute:
		return fmt.Sprintf("~%ds left", int(d.Round(5*time.Second).Seconds()))
	case d < 10*time.Minute:
		d = d.Round(10 * time.Second)
		return fmt.Sprintf("~%dm%02ds left", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("~%dm left", int(d.Round(time.Minute).Minutes()))
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("~%dh%02dm left", int(d.Hours()), int(d.Minutes())%60)
}

// clock is an elapsed time: 42s, 6m02s, 1h04m.
func clock(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// compact is a count in a few characters: 950, 41.2K, 86M, 1.9B.
func compact(v float64) string {
	for _, u := range []struct {
		div  float64
		unit string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= u.div {
			x := v / u.div
			if x >= 100 {
				return fmt.Sprintf("%.0f%s", x, u.unit)
			}
			return fmt.Sprintf("%.1f%s", x, u.unit)
		}
	}
	return fmt.Sprintf("%.0f", v)
}

// bytesText is a size in a few characters: 950 B, 2.1 MB.
func bytesText(n int) string {
	v := float64(n)
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1f MB", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1f KB", v/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

// streamView is the interpreter phase's live state: progress in steps,
// measured against the length of earlier runs when there are any.
type streamView struct {
	mu     sync.Mutex
	t0     time.Time
	est    history.Estimate
	hasEst bool
	pr     stepkvm.Progress
	steps  rate
	cd     countdown
}

func newStreamView(est history.Estimate, ok bool) *streamView {
	return &streamView{t0: time.Now(), est: est, hasEst: ok, steps: rate{window: 90 * time.Second}}
}

func (v *streamView) update(pr stepkvm.Progress) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	v.pr = pr
	v.steps.add(now, float64(pr.Steps))
	if v.hasEst && pr.Steps < v.est.Steps {
		v.cd.update(now, float64(v.est.Steps-pr.Steps), v.steps.perSecond())
	}
}

// expected is the length the bar measures against, and how it is known.
func (v *streamView) expected() string {
	switch {
	case v.est.Exact:
		return "known from an earlier run"
	case v.est.Tokens == 1:
		return "estimated from 1 token"
	}
	return fmt.Sprintf("estimated from %d tokens (%s–%s)",
		v.est.Tokens, compact(float64(v.est.Lo)), compact(float64(v.est.Hi)))
}

func (v *streamView) view() (float64, string, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	pr := v.pr
	elapsed := clock(now.Sub(v.t0))
	sizes := fmt.Sprintf("%s · %s calls · %s", bytesText(pr.StreamBytes), commas(uint64(pr.Calls)), elapsed)
	speed := ""
	if r := v.steps.perSecond(); r > 0 {
		speed = " · " + compact(r) + " steps/s"
	}
	if pr.Steps == 0 {
		return -1, "starting the interpreter", elapsed
	}
	if !v.hasEst {
		return -1, compact(float64(pr.Steps)) + " steps",
			sizes + speed + " · no estimate yet (first run of this piece)"
	}
	if pr.Steps >= v.est.Steps {
		// Longer than expected: the estimate no longer says anything.
		return -1, compact(float64(pr.Steps)) + " steps, past the estimate",
			sizes + speed + " · " + v.expected()
	}
	frac := min(float64(pr.Steps)/float64(v.est.Steps), 0.99)
	head := fmt.Sprintf("%d%%", int(frac*100))
	if left, ok := v.cd.left(now); ok {
		head += " · " + leftText(left)
	} else {
		head += " · estimating time left"
	}
	if !v.est.Exact {
		head = "~" + head
	}
	return frac, head, fmt.Sprintf("%s of %s steps · %s%s · %s",
		compact(float64(pr.Steps)), compact(float64(v.est.Steps)), sizes, speed, v.expected())
}

// rasterView is the renderer phase's live state. Its total is known: every
// drawing record of the stream, in every band it touches.
type rasterView struct {
	mu  sync.Mutex
	t0  time.Time
	pr  stamp.Progress
	ops rate
	cd  countdown
}

func newRasterView() *rasterView {
	return &rasterView{t0: time.Now(), ops: rate{window: 30 * time.Second}}
}

func (v *rasterView) update(pr stamp.Progress) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	v.pr = pr
	v.ops.add(now, float64(pr.OpsDone))
	if pr.OpsAll > pr.OpsDone {
		v.cd.update(now, float64(pr.OpsAll-pr.OpsDone), v.ops.perSecond())
	}
}

func (v *rasterView) view() (float64, string, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	pr := v.pr
	if pr.OpsAll == 0 {
		return -1, "starting the renderer", clock(now.Sub(v.t0))
	}
	frac := math.Min(float64(pr.OpsDone)/float64(pr.OpsAll), 1)
	head := fmt.Sprintf("%d%%", int(frac*100))
	if left, ok := v.cd.left(now); ok {
		head += " · " + leftText(left)
	} else {
		head += " · estimating time left"
	}
	return frac, head, fmt.Sprintf("bands %d/%d · ops %s/%s · %s calls · %s",
		pr.BandsDone, pr.Bands, compact(float64(pr.OpsDone)), compact(float64(pr.OpsAll)),
		commas(uint64(pr.Calls)), clock(now.Sub(v.t0)))
}

// spinView is a live region for a wait of unknown length (a download, a
// node starting): the text and a clock.
func spinView(text string) func() (float64, string, string) {
	t0 := time.Now()
	return func() (float64, string, string) {
		return -1, text, clock(time.Since(t0))
	}
}

// short abbreviates a hex address or hash for transcript rows.
func short(s string) string {
	if len(s) <= 14 {
		return s
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// joinDot joins the non-empty parts with " · ".
func joinDot(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

// shortRef abbreviates a reference for a transcript row, keeping its shape:
// ipfs://bafybe…ud2m/1.png, example.org/…/1.png.
func shortRef(s string) string {
	if rest, ok := strings.CutPrefix(s, "ipfs://"); ok {
		cid, path, _ := strings.Cut(rest, "/")
		if path != "" {
			path = "/" + path
		}
		if len(cid) > 14 {
			cid = cid[:6] + "…" + cid[len(cid)-4:]
		}
		return "ipfs://" + cid + path
	}
	if i := strings.Index(s, "://"); i > 0 {
		host, path, _ := strings.Cut(s[i+3:], "/")
		if j := strings.LastIndex(path, "/"); j >= 0 {
			return host + "/…" + path[j:]
		}
		return host + "/" + path
	}
	return short(s)
}

// hostOf is the host of a URL, or the URL when it has none.
func hostOf(u string) string {
	if i := strings.Index(u, "://"); i > 0 {
		host, _, _ := strings.Cut(u[i+3:], "/")
		return host
	}
	return u
}
