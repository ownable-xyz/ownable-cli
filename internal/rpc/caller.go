// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package rpc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// GasMeter selects how the gas of each successful call is measured.
type GasMeter int

const (
	// MeterAuto uses debug_traceCall when the node has it, else eth_estimateGas.
	MeterAuto GasMeter = iota
	// MeterTrace uses debug_traceCall only.
	MeterTrace
	// MeterEstimate uses eth_estimateGas only.
	MeterEstimate
	// MeterOff does not measure; gas totals are reported as unknown.
	MeterOff
)

// estimateCeiling is the EIP-7825 transaction gas limit (2^24). Many nodes
// cap eth_estimateGas there, so an estimate at or above it is not trusted.
const estimateCeiling = 1 << 24

// Caller makes eth_calls under one gas cap and keeps the totals that the
// render report prints. It is safe for concurrent use.
//
// The proof that a render fits the cap is the eth_call itself: every call is
// sent with gas = Cap, and a call that needs more fails in the EVM. Gas
// measurement is a separate, optional read that only feeds the report and the
// chunk sizing.
type Caller struct {
	Client *Client
	Cap    uint64
	Block  string
	Meter  GasMeter
	// Every measures one call in Every (after the first few, which are all
	// measured); 1 measures every call, for exact totals. Measuring costs
	// one extra request per measured call.
	Every int

	seen     atomic.Int64 // calls considered for measurement
	calls    atomic.Int64 // successful calls under the cap
	misses   atomic.Int64 // calls that failed in the EVM (did not fit, or reverted)
	gas      atomic.Uint64
	measured atomic.Int64
	rejected atomic.Int64 // estimates discarded at the node's estimate cap
	maxGas   atomic.Uint64

	pending sync.WaitGroup
	sem     chan struct{}

	mu       sync.Mutex
	traceBad int  // consecutive debug_traceCall failures
	traceOff bool // the node has no usable debug_traceCall
	how      string
}

// NewCaller returns a Caller at the latest block.
func NewCaller(c *Client, cap uint64, meter GasMeter) *Caller {
	return &Caller{Client: c, Cap: cap, Block: "latest", Meter: meter, Every: 1, sem: make(chan struct{}, 4)}
}

// Call performs eth_call under the cap without measuring gas.
func (c *Caller) Call(ctx context.Context, to string, data []byte) ([]byte, error) {
	ret, err := c.Client.EthCall(ctx, to, data, c.Cap, c.Block)
	if err != nil {
		if _, ok := err.(*RevertError); ok {
			c.misses.Add(1)
		}
		return nil, err
	}
	c.calls.Add(1)
	return ret, nil
}

// CallMeasured performs eth_call under the cap and, on success, measures the
// call's gas. gas is 0 when it could not be measured.
func (c *Caller) CallMeasured(ctx context.Context, to string, data []byte) ([]byte, uint64, error) {
	ret, err := c.Call(ctx, to, data)
	if err != nil {
		return nil, 0, err
	}
	if !c.sample() {
		return ret, 0, nil
	}
	gas := c.measure(ctx, to, data)
	c.record(gas)
	return ret, gas, nil
}

// sample decides whether this call is measured.
func (c *Caller) sample() bool {
	if c.Meter == MeterOff {
		return false
	}
	n := c.seen.Add(1)
	return c.Every <= 1 || n <= 8 || n%int64(c.Every) == 0
}

// CallMeasureAsync performs eth_call under the cap and measures its gas in the
// background, so the measurement does not slow the chain of calls down. The
// returned channel receives the gas (0 if unmeasured) once known. Call Wait
// before reading Stats.
func (c *Caller) CallMeasureAsync(ctx context.Context, to string, data []byte) ([]byte, <-chan uint64, error) {
	ret, err := c.Call(ctx, to, data)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan uint64, 1)
	if !c.sample() {
		ch <- 0
		return ret, ch, nil
	}
	select {
	case c.sem <- struct{}{}:
	default:
		// Measurements are falling behind: skip this one rather than slow
		// the chain of calls down.
		ch <- 0
		return ret, ch, nil
	}
	c.pending.Add(1)
	go func() {
		defer c.pending.Done()
		defer func() { <-c.sem }()
		gas := c.measure(ctx, to, data)
		c.record(gas)
		ch <- gas
	}()
	return ret, ch, nil
}

// Wait blocks until every background measurement has finished.
func (c *Caller) Wait() { c.pending.Wait() }

func (c *Caller) record(gas uint64) {
	if gas == 0 {
		return
	}
	c.gas.Add(gas)
	c.measured.Add(1)
	for {
		m := c.maxGas.Load()
		if gas <= m || c.maxGas.CompareAndSwap(m, gas) {
			break
		}
	}
}

func (c *Caller) measure(ctx context.Context, to string, data []byte) uint64 {
	if c.Meter == MeterOff {
		return 0
	}
	c.mu.Lock()
	useTrace := (c.Meter == MeterAuto || c.Meter == MeterTrace) && !c.traceOff
	c.mu.Unlock()
	if useTrace {
		if g, ok := c.Client.TraceCallGas(ctx, to, data, c.Cap, c.Block); ok {
			c.mu.Lock()
			c.traceBad = 0
			c.how = "debug_traceCall"
			c.mu.Unlock()
			return g
		}
		debugf("debug_traceCall gave no gas for a call to %s", to)
		c.mu.Lock()
		c.traceBad++
		if c.traceBad >= 2 {
			c.traceOff = true
		}
		c.mu.Unlock()
		if c.Meter == MeterTrace {
			return 0
		}
	}
	if c.Meter == MeterTrace {
		return 0
	}
	g, err := c.Client.EstimateGas(ctx, to, data, c.Cap, c.Block)
	if err != nil {
		debugf("eth_estimateGas failed for a call to %s: %v", to, err)
		var rev *RevertError
		if errors.As(err, &rev) && rev.OutOfGas() {
			// The call fits the eth_call cap but not the node's estimate
			// cap: estimates cannot measure calls this large.
			c.rejected.Add(1)
		}
		return 0
	}
	if g >= estimateCeiling {
		// Some nodes cap eth_estimateGas at the EIP-7825 transaction limit
		// and answer with the cap itself; that is not a measurement.
		debugf("eth_estimateGas answered %d (the node's estimate cap) for a call to %s", g, to)
		c.rejected.Add(1)
		return 0
	}
	c.mu.Lock()
	if c.how == "" {
		c.how = "eth_estimateGas"
	}
	c.mu.Unlock()
	return g
}

// Stats is a snapshot of the totals.
type Stats struct {
	Calls    int64  // successful calls, every one under the cap
	Misses   int64  // calls that failed in the EVM and were retried smaller
	Gas      uint64 // sum of measured gas
	Measured int64  // how many calls were measured
	MaxGas   uint64 // largest measured call
	How      string // the measuring method, "" if none
	Rejected int64  // estimates discarded at the node's estimate cap
}

// Stats returns the current totals.
func (c *Caller) Stats() Stats {
	c.mu.Lock()
	how := c.how
	c.mu.Unlock()
	return Stats{
		Calls: c.calls.Load(), Misses: c.misses.Load(), Gas: c.gas.Load(),
		Measured: c.measured.Load(), MaxGas: c.maxGas.Load(), How: how, Rejected: c.rejected.Load(),
	}
}

var debug = os.Getenv("OWNABLE_DEBUG") != ""

func debugf(format string, a ...any) {
	if debug {
		fmt.Fprintf(os.Stderr, "\n[debug] "+format+"\n", a...)
	}
}

// Estimate is the total gas: exact when every call was measured, otherwise
// the measured sum scaled up to all calls.
//
// When the node's eth_estimateGas cap discarded some measurements, the
// measured calls are only the small ones and say nothing about the rest, so
// there is no estimate.
func (s Stats) Estimate() (gas uint64, exact bool) {
	if s.Measured == 0 || s.Rejected > 0 {
		return 0, false
	}
	if s.Measured >= s.Calls {
		return s.Gas, true
	}
	return uint64(float64(s.Gas) * float64(s.Calls) / float64(s.Measured)), false
}
