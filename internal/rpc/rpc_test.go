// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ownable-xyz/ownable-cli/internal/fakechain"
)

func TestEstimate(t *testing.T) {
	if g, exact := (Stats{Calls: 10, Measured: 10, Gas: 500}).Estimate(); g != 500 || !exact {
		t.Fatalf("all measured: %d %v", g, exact)
	}
	if g, exact := (Stats{Calls: 10, Measured: 2, Gas: 100}).Estimate(); g != 500 || exact {
		t.Fatalf("sampled: %d %v", g, exact)
	}
	if g, _ := (Stats{Calls: 10, Measured: 2, Gas: 100, Rejected: 1}).Estimate(); g != 0 {
		t.Fatal("an estimate built on capped measurements")
	}
}

// A rate-limited endpoint is waited out; a revert is returned as RevertError
// and never retried.
func TestRetriesAndReverts(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case 2:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32005, "message": "Too Many Requests"}})
		case 3:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x2a"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32000, "message": "out of gas"}})
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	ret, err := c.EthCall(context.Background(), "0x0000000000000000000000000000000000000001", nil, 1, "latest")
	if err != nil || len(ret) != 1 || ret[0] != 0x2a {
		t.Fatalf("got %x, %v", ret, err)
	}
	if c.Throttled.Load() != 2 {
		t.Fatalf("throttled %d, want 2", c.Throttled.Load())
	}
	_, err = c.EthCall(context.Background(), "0x0000000000000000000000000000000000000001", nil, 1, "latest")
	var rev *RevertError
	if !errors.As(err, &rev) || !rev.OutOfGas() {
		t.Fatalf("want an out-of-gas RevertError, got %v", err)
	}
	if n.Load() != 4 {
		t.Fatalf("a revert was retried (%d requests)", n.Load())
	}
}

// On a node without debug_traceCall whose eth_estimateGas stops at 2^24, big
// calls cannot be measured, and the report must not extrapolate from the
// small calls that could be.
func TestCappedEstimatesAreNotExtrapolated(t *testing.T) {
	chain := fakechain.New()
	defer chain.Close()
	chain.NoTrace = true
	chain.EstCap = 1 << 24
	const addr = "0x0000000000000000000000000000000000000abc"
	gasFor := map[byte]uint64{1: 1_000_000, 2: 40_000_000}
	chain.Handle(addr, func(data []byte) ([]byte, uint64, error) { return nil, gasFor[data[0]], nil })
	c := NewCaller(New(chain.URL()), 50_000_000, MeterAuto)
	for _, d := range []byte{1, 2, 1} {
		if _, _, err := c.CallMeasured(context.Background(), addr, []byte{d}); err != nil {
			t.Fatal(err)
		}
	}
	st := c.Stats()
	if st.Measured != 2 || st.Rejected != 1 {
		t.Fatalf("measured %d, rejected %d", st.Measured, st.Rejected)
	}
	if g, _ := st.Estimate(); g != 0 {
		t.Fatalf("extrapolated %d from capped estimates", g)
	}
}
