// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package history remembers how long each finished interpreter run was, so
// a later render can say how far along it is.
//
// The length of a KVM run is unknown until the program halts: nothing in the
// program or the stream declares it. What is known is the length of runs
// already finished. The same token (same interpreter, program, seed and
// params) runs exactly as many steps every time, and the tokens of one
// piece share a program, so their lengths are a fair guide to each other.
// The record is a small JSON file in the user's cache directory; it holds
// counts only, and deleting it loses nothing but the estimates.
package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Run is one finished interpreter run.
type Run struct {
	Key         string    `json:"key"`     // stepkvm.RunKey, hex
	Program     string    `json:"program"` // sha256 of the program, hex
	Steps       uint64    `json:"steps"`
	StreamBytes int       `json:"streamBytes"`
	Calls       int       `json:"calls"`
	When        time.Time `json:"when"`
}

// Known are runs measured on mainnet and shipped with the tool, so the first
// render of a known piece on a new machine already has an estimate. They
// count as other tokens of their program, never as the run itself.
var Known = []Run{
	// Rakel #1 (0xe56522Ebaf8E19c7E2B9468Cbef8aF14773F5F18), 2026-10-05.
	{Key: "known:rakel-1", Program: "f460823c82e3a50ff380eff8930a42e511222f179913706bd5da853f65509c58",
		Steps: 209_645_651, StreamBytes: 3_416_266, Calls: 4467},
}

// maxRuns bounds the file; the oldest runs are dropped first.
const maxRuns = 1000

// Store is the history file. A nil Store records and recalls nothing.
type Store struct{ path string }

// Default is the store under the user's cache directory, or nil when there
// is none.
func Default() *Store {
	d, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	return &Store{path: filepath.Join(d, "ownable", "runs.json")}
}

// At is a store at path (for tests).
func At(path string) *Store { return &Store{path: path} }

// Load returns the recorded runs; a missing or unreadable file is empty.
func (s *Store) Load() []Run {
	if s == nil {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var runs []Run
	if json.Unmarshal(b, &runs) != nil {
		return nil
	}
	return runs
}

// Add records r, replacing an earlier run with the same key.
func (s *Store) Add(r Run) error {
	if s == nil {
		return nil
	}
	runs := slices.DeleteFunc(s.Load(), func(o Run) bool { return o.Key == r.Key })
	runs = append(runs, r)
	if len(runs) > maxRuns {
		runs = runs[len(runs)-maxRuns:]
	}
	b, err := json.MarshalIndent(runs, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Estimate is the expected length of a run.
type Estimate struct {
	Steps  uint64 // expected total steps
	Exact  bool   // this very run finished before: Steps is its length
	Tokens int    // otherwise, how many other runs of the program it is drawn from
	Lo, Hi uint64 // their shortest and longest
}

// Lookup estimates the run with the given key and program: exactly when it
// finished before, else the median of the program's other runs, the Known
// runs included. ok is false when nothing is known.
func Lookup(runs []Run, key, program string) (e Estimate, ok bool) {
	var steps []uint64
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.Key] = true
	}
	for _, r := range Known {
		if !seen[r.Key] {
			runs = append(runs[:len(runs):len(runs)], r)
		}
	}
	for _, r := range runs {
		if r.Key == key && r.Steps > 0 {
			return Estimate{Steps: r.Steps, Exact: true}, true
		}
		if r.Program == program && r.Steps > 0 {
			steps = append(steps, r.Steps)
		}
	}
	if len(steps) == 0 {
		return Estimate{}, false
	}
	slices.Sort(steps)
	return Estimate{Steps: steps[len(steps)/2], Tokens: len(steps), Lo: steps[0], Hi: steps[len(steps)-1]}, true
}
