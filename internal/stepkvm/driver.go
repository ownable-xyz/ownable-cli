// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package stepkvm runs a KVM program through the deployed stepped
// interpreter (KVMStepRunner.executeChunk) in a chain of eth_calls, each one
// under the node's gas cap. Between calls it threads the snapshot the chain
// returns, while three pieces of growable state stay with the client: the
// noise table, the linear-memory image and the accumulated open path. The
// contract's invariant is that concatenating the chunk streams, for any chunk
// boundaries, gives the same draw stream as one KVMRunner.execute call.
package stepkvm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
)

// Progress is live telemetry for the terminal.
type Progress struct {
	Phase       string // "noise" or "chunk"
	Calls       int
	Budget      uint64
	StreamBytes int
	Steps       uint64
	Done        bool
}

// Config describes one stream acquisition.
type Config struct {
	Caller        *rpc.Caller
	StepRunner    string // KVMStepRunner address
	NoiseLib      string // NoiseV1Ext address ("" = do not build the noise table)
	Program       []byte
	Seed          int32
	Params        []int64
	InitialBudget uint64 // step budget to start from (default 30000)
	MaxBudget     uint64 // largest step budget per call (default 4,000,000)
	OnProgress    func(Progress)
	// Checkpoint, when set, is a file the chain's progress is saved to every
	// few seconds and on interruption, and resumed from when it exists. When
	// the stream is complete it is removed, or, with KeepDone, rewritten as a
	// complete checkpoint that the next run returns at once (the caller
	// removes it when it no longer needs the stream).
	Checkpoint string
	KeepDone   bool
	// OnResume is called when a checkpoint was loaded.
	OnResume func(calls int, streamBytes int)
}

// Result is the assembled draw stream plus telemetry.
type Result struct {
	Stream     []byte
	Calls      int // executeChunk calls that succeeded
	NoiseCalls int // buildTableRange calls
	Retries    int // calls that did not fit and were retried with a smaller budget
	Steps      uint64
	Resumed    int // calls carried over from a checkpoint (included in Calls)
}

const (
	minBudget     = 1_000
	maxBudget     = 4_000_000
	noiseEntryGas = 23_400 // gas per noise-table entry in buildTableRange
	blindGrowGap  = 4      // without a gas measurement: grow x1.5 after this many clean calls
)

// NoiseTableBytes is the size of the client-held noise table: 4096 entries of
// 8 bytes (big-endian int64), a pure function of the run seed.
const NoiseTableBytes = 4096 * 8

// AcquireNoiseTable builds the 4096-entry noise table from the deployed
// NoiseV1Ext with chained buildTableRange calls, threading the generator
// state between slices so every call stays under the cap.
func AcquireNoiseTable(ctx context.Context, c *rpc.Caller, noiseLib string, seed int32) ([]byte, int, error) {
	blob := make([]byte, NoiseTableBytes)
	g := new(big.Int).SetUint64(uint64(uint32(seed)))
	// Size slices from the cap at about 23.4k gas per entry, with headroom.
	slice := int(min(max(c.Cap*80/100/noiseEntryGas, 16), 4096))
	built, calls := 0, 0
	for built < 4096 {
		count := min(4096-built, slice)
		ret, err := c.Call(ctx, noiseLib, abi.EncodeBuildTableRange(g, uint64(count)))
		if err != nil {
			var rev *rpc.RevertError
			if errors.As(err, &rev) && count > 1 {
				slice = max(count/2, 1) // did not fit: halve and retry
				continue
			}
			return nil, calls, fmt.Errorf("noise table slice at %d: %w", built, err)
		}
		calls++
		entries, gOut, err := abi.DecodeBuildTableRange(ret)
		if err != nil {
			return nil, calls, fmt.Errorf("noise table decode: %w", err)
		}
		if len(entries) != count {
			return nil, calls, fmt.Errorf("noise table slice returned %d entries, want %d", len(entries), count)
		}
		for i, v := range entries {
			binary.BigEndian.PutUint64(blob[(built+i)*8:], uint64(v))
		}
		g = gOut
		built += count
	}
	return blob, calls, nil
}

// Stream runs the chunk chain to completion and returns the draw stream.
//
// The step budget tunes itself toward about 90% of the cap from the measured
// gas of each call when the node can measure, and by cautious growth after a
// run of clean calls when it cannot. A chunk that does not fit halves the
// budget and is retried from the same snapshot: the lane is resumable, so a
// retry loses nothing.
func Stream(ctx context.Context, cfg *Config) (_ *Result, err error) {
	res := &Result{}
	var (
		noise, memImg, pathBlob, snapshot, stream []byte
		budget                                    = cfg.InitialBudget
	)
	if budget == 0 {
		budget = 30_000
	}
	budgetCap := uint64(maxBudget)
	if cfg.MaxBudget > 0 {
		budgetCap = cfg.MaxBudget
	}
	budget = min(budget, budgetCap)
	key := RunKey(cfg.StepRunner, cfg.Program, cfg.Seed, cfg.Params)
	if cfg.Checkpoint != "" {
		st, err := LoadCheckpoint(cfg.Checkpoint, key)
		switch {
		case err == nil:
			snapshot, memImg, pathBlob, noise, stream = st.Snapshot, st.MemImg, st.PathBlob, st.Noise, st.Stream
			res.Calls, res.NoiseCalls, res.Retries, res.Steps, res.Resumed = st.Calls, st.NoiseCalls, st.Retries, st.Steps, st.Calls
			if st.Budget > 0 {
				budget = st.Budget
			}
			if len(noise) == 0 {
				noise = nil
			}
			if cfg.OnResume != nil {
				cfg.OnResume(st.Calls, len(st.Stream))
			}
			if st.Done {
				res.Stream = stream
				return res, nil
			}
		case errors.Is(err, ErrNoCheckpoint):
		default:
			return nil, err
		}
	}
	lastSave := time.Now()
	finished := false
	save := func() error {
		if cfg.Checkpoint == "" {
			return nil
		}
		lastSave = time.Now()
		st := &State{Snapshot: snapshot, MemImg: memImg, PathBlob: pathBlob,
			Noise: noise, Stream: stream, Calls: res.Calls, NoiseCalls: res.NoiseCalls, Retries: res.Retries,
			Steps: res.Steps, Budget: budget, Done: finished}
		if finished {
			st.Snapshot, st.MemImg, st.PathBlob, st.Noise = nil, nil, nil, nil
		}
		return SaveCheckpoint(cfg.Checkpoint, key, st)
	}
	defer func() {
		// Keep the progress made so far when the run stops early.
		if err != nil && cfg.Checkpoint != "" && (res.Calls > res.Resumed || noise != nil) {
			if serr := save(); serr != nil {
				err = fmt.Errorf("%w (and the checkpoint could not be saved: %v)", err, serr)
			}
		}
	}()
	if cfg.NoiseLib != "" && noise == nil {
		var err error
		noise, res.NoiseCalls, err = AcquireNoiseTable(ctx, cfg.Caller, cfg.NoiseLib, cfg.Seed)
		if err != nil {
			return nil, err
		}
		if cfg.OnProgress != nil {
			cfg.OnProgress(Progress{Phase: "noise", Calls: res.NoiseCalls})
		}
	}

	target := cfg.Caller.Cap * 90 / 100
	type measurement struct {
		budget uint64
		gas    <-chan uint64
	}
	var (
		cleanCalls  int
		ceiling     uint64
		pending     []measurement
		measuredAny bool
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Fold in any gas measurements that have arrived: retune the budget
		// from the newest one, scaled from the budget that call ran with.
		for len(pending) > 0 {
			var g uint64
			select {
			case g = <-pending[0].gas:
			default:
				g = ^uint64(0)
			}
			if g == ^uint64(0) {
				break
			}
			if g > 0 {
				measuredAny = true
				f := min(max(float64(target)/float64(g), 0.25), 4)
				budget = min(max(uint64(float64(pending[0].budget)*f), minBudget), budgetCap)
			}
			pending = pending[1:]
		}
		var ret []byte
		var gasCh <-chan uint64
		var err error
		for {
			cd := abi.EncodeExecuteChunk(cfg.Program, cfg.Seed, cfg.Params, snapshot,
				[][]byte{memImg, noise, pathBlob}, budget)
			ret, gasCh, err = cfg.Caller.CallMeasureAsync(ctx, cfg.StepRunner, cd)
			if err == nil {
				break
			}
			var rev *rpc.RevertError
			if !errors.As(err, &rev) {
				return nil, fmt.Errorf("executeChunk call %d: %w", res.Calls+1, err)
			}
			if budget <= 1 {
				return nil, fmt.Errorf("executeChunk call %d fails even with a budget of 1 step under a %d gas cap: %w",
					res.Calls+1, cfg.Caller.Cap, rev)
			}
			if ceiling == 0 || budget < ceiling {
				ceiling = budget // the smallest budget known not to fit
			}
			budget = max(budget/2, 1) // did not fit: halve and retry from the same snapshot
			res.Retries++
			cleanCalls = 0
		}
		out, err := abi.DecodeExecuteChunk(ret)
		if err != nil {
			return nil, fmt.Errorf("executeChunk call %d decode: %w", res.Calls+1, err)
		}
		res.Calls++

		pending = append(pending, measurement{budget: budget, gas: gasCh})
		if !measuredAny {
			cleanCalls++
			if cleanCalls >= blindGrowGap {
				// Without measurements, grow after clean calls, but stay just
				// under the smallest budget that did not fit (which itself
				// drifts up slowly, since the gas per step varies).
				nb := budget * 3 / 2
				if ceiling > 0 {
					ceiling += ceiling / 50
					nb = min(nb, ceiling*9/10)
				}
				budget = min(max(nb, minBudget), budgetCap)
				cleanCalls = 0
			}
		}

		stream = append(stream, out.ChunkStream...)
		if memImg, err = ApplyMemDelta(memImg, out.MemDelta); err != nil {
			return nil, fmt.Errorf("call %d: %w", res.Calls, err)
		}
		if pathBlob, err = ApplyPathDelta(pathBlob, out.PathDelta); err != nil {
			return nil, fmt.Errorf("call %d: %w", res.Calls, err)
		}
		snapshot = out.NextSnapshot

		if !out.Done {
			if len(snapshot) == 0 {
				return nil, fmt.Errorf("call %d: executeChunk returned an empty snapshot before done", res.Calls)
			}
			// Validate what the chain handed back before using it as resume
			// state: snapshot structure, path-blob consistency, memory geometry.
			snap, err := DecodeSnapshot(snapshot)
			if err != nil {
				return nil, fmt.Errorf("call %d: snapshot invalid: %w", res.Calls, err)
			}
			res.Steps = snap.Steps
			cx, cy, hasCur, err := WalkPathBlob(pathBlob, snap.PathSegs)
			if err != nil {
				return nil, fmt.Errorf("call %d: %w", res.Calls, err)
			}
			if hasCur != snap.HasCurrent() {
				return nil, fmt.Errorf("call %d: snapshot hasCurrent=%v but the path blob says %v", res.Calls, snap.HasCurrent(), hasCur)
			}
			if hasCur && (cx != snap.CurX || cy != snap.CurY) {
				return nil, fmt.Errorf("call %d: snapshot current point (%d,%d) != path blob (%d,%d)", res.Calls, snap.CurX, snap.CurY, cx, cy)
			}
			if snap.HasMemory() {
				if len(memImg) == 0 {
					memImg = make([]byte, int(snap.MemBound)*8) // allocated, nothing written yet
				} else if len(memImg) != int(snap.MemBound)*8 {
					return nil, fmt.Errorf("call %d: memory image %d B != memBound %d x 8", res.Calls, len(memImg), snap.MemBound)
				}
			}
			if snap.Flags&FlagNoiseTable != 0 && noise == nil {
				return nil, fmt.Errorf("call %d: the program initialised the kind-0 noise table but no noise library is known (pass --noise-lib)", res.Calls)
			}
		}
		if cfg.OnProgress != nil {
			cfg.OnProgress(Progress{Phase: "chunk", Calls: res.Calls, Budget: budget,
				StreamBytes: len(stream), Steps: res.Steps, Done: out.Done})
		}
		if out.Done {
			break
		}
		if cfg.Checkpoint != "" && time.Since(lastSave) > 15*time.Second {
			if err := save(); err != nil {
				return nil, fmt.Errorf("saving checkpoint: %w", err)
			}
		}
	}
	if cfg.Checkpoint != "" {
		if cfg.KeepDone {
			finished = true
			if err := save(); err != nil {
				return nil, fmt.Errorf("saving checkpoint: %w", err)
			}
		} else {
			os.Remove(cfg.Checkpoint)
		}
	}
	res.Stream = stream
	return res, nil
}
