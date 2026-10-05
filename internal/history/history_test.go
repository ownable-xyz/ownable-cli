// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package history

import (
	"path/filepath"
	"testing"
)

func TestLookup(t *testing.T) {
	runs := []Run{
		{Key: "a", Program: "p", Steps: 100},
		{Key: "b", Program: "p", Steps: 300},
		{Key: "c", Program: "p", Steps: 200},
		{Key: "d", Program: "q", Steps: 5},
	}
	if e, ok := Lookup(runs, "b", "p"); !ok || !e.Exact || e.Steps != 300 {
		t.Fatalf("same run: got %+v %v", e, ok)
	}
	e, ok := Lookup(runs, "z", "p")
	if !ok || e.Exact || e.Steps != 200 || e.Tokens != 3 || e.Lo != 100 || e.Hi != 300 {
		t.Fatalf("sibling runs: got %+v %v", e, ok)
	}
	if _, ok := Lookup(runs, "z", "r"); ok {
		t.Fatal("unknown program: want no estimate")
	}
}

func TestLookupUsesKnownRuns(t *testing.T) {
	k := Known[0]
	e, ok := Lookup(nil, "some other token", k.Program)
	if !ok || e.Exact || e.Steps != k.Steps || e.Tokens != 1 {
		t.Fatalf("known program, empty history: got %+v %v", e, ok)
	}
	// The user's own runs join the known ones.
	e, _ = Lookup([]Run{{Key: "mine", Program: k.Program, Steps: 1}}, "x", k.Program)
	if e.Tokens != 2 || e.Lo != 1 || e.Hi != k.Steps {
		t.Fatalf("known plus own: got %+v", e)
	}
}

func TestStoreReplacesSameKey(t *testing.T) {
	s := At(filepath.Join(t.TempDir(), "sub", "runs.json"))
	if len(s.Load()) != 0 {
		t.Fatal("missing file: want no runs")
	}
	for _, r := range []Run{{Key: "a", Steps: 1}, {Key: "b", Steps: 2}, {Key: "a", Steps: 3}} {
		if err := s.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	runs := s.Load()
	if len(runs) != 2 || runs[1].Key != "a" || runs[1].Steps != 3 {
		t.Fatalf("got %+v", runs)
	}
	var nilStore *Store
	if nilStore.Add(Run{Key: "x"}) != nil || nilStore.Load() != nil {
		t.Fatal("nil store: want a no-op")
	}
}
