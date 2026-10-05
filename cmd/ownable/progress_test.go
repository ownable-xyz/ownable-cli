// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/history"
	"github.com/ownable-xyz/ownable-cli/internal/stepkvm"
)

func TestRateUsesTheWindow(t *testing.T) {
	r := rate{window: 10 * time.Second}
	t0 := time.Now()
	// A slow start (10/s), then a steady 100/s.
	v := 0.0
	for i := 0; i <= 30; i++ {
		if i > 10 {
			v += 100
		} else {
			v += 10
		}
		r.add(t0.Add(time.Duration(i)*time.Second), v)
	}
	if got := r.perSecond(); got < 99 || got > 101 {
		t.Fatalf("rate = %v, want 100 (the slow start is outside the window)", got)
	}
	if (&rate{window: time.Minute}).perSecond() != 0 {
		t.Fatal("no samples: want 0")
	}
}

func TestCountdownRunsDown(t *testing.T) {
	var c countdown
	now := time.Now()
	if _, ok := c.left(now); ok {
		t.Fatal("no estimate yet")
	}
	c.update(now, 600, 10) // 60s left
	left, _ := c.left(now.Add(20 * time.Second))
	if left != 40*time.Second {
		t.Fatalf("left = %v, want 40s", left)
	}
	if left, _ := c.left(now.Add(time.Hour)); left != 0 {
		t.Fatalf("past the finish: left = %v, want 0", left)
	}
}

func TestLeftText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Second:                 "a few seconds left",
		42 * time.Second:                "~40s left",
		4*time.Minute + 7*time.Second:   "~4m10s left",
		23*time.Minute + 20*time.Second: "~23m left",
		time.Hour + 4*time.Minute:       "~1h04m left",
	} {
		if got := leftText(d); got != want {
			t.Errorf("leftText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestStreamView(t *testing.T) {
	// Nothing known: no fill, and it says so.
	v := newStreamView(history.Estimate{}, false)
	v.update(stepkvm.Progress{Calls: 3, Steps: 5_000_000, StreamBytes: 1000})
	if frac, _, detail := v.view(); frac >= 0 || !strings.Contains(detail, "no estimate") {
		t.Fatalf("no history: got %v %q", frac, detail)
	}
	// An earlier run of the same token: an exact fraction.
	v = newStreamView(history.Estimate{Steps: 20_000_000, Exact: true}, true)
	v.update(stepkvm.Progress{Calls: 3, Steps: 5_000_000})
	if frac, head, _ := v.view(); frac != 0.25 || !strings.HasPrefix(head, "25%") {
		t.Fatalf("exact: got %v %q", frac, head)
	}
	// Past an estimate drawn from other tokens: back to indeterminate.
	v = newStreamView(history.Estimate{Steps: 1_000_000, Tokens: 2}, true)
	v.update(stepkvm.Progress{Calls: 3, Steps: 2_000_000})
	if frac, head, _ := v.view(); frac >= 0 || !strings.Contains(head, "past the estimate") {
		t.Fatalf("past: got %v %q", frac, head)
	}
}

func TestShortRef(t *testing.T) {
	for in, want := range map[string]string{
		"ipfs://bafybeifhe3zwculpcnkm7pvd75axjonsqfuc3eirjiv2lzotjl4pxwud2m/1.png": "ipfs://bafybe…ud2m/1.png",
		"ipfs://QmShort/1.png":                  "ipfs://QmShort/1.png",
		"https://example.org/rakel/works/1.png": "example.org/…/1.png",
		"https://example.org/1.png":             "example.org/1.png",
		"rakel-1.png":                           "rakel-1.png",
	} {
		if got := shortRef(in); got != want {
			t.Errorf("shortRef(%q) = %q, want %q", in, got, want)
		}
	}
	if got := hostOf("https://ipfs.filebase.io/ipfs/"); got != "ipfs.filebase.io" {
		t.Errorf("hostOf = %q", got)
	}
}
