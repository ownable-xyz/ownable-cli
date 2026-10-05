// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package stepkvm

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/fakechain"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
)

const (
	stepAddr  = "0x1d8a97a40DFc234578cb6F4aa13a99826B67e228"
	noiseAddr = "0x45bf3Ee5dA57b922D93125991E45F444A6E4B28E"
)

// bytesArray decodes a bytes[] argument at slot i of calldata (after the selector).
func bytesArray(t *testing.T, args []byte, slot int) [][]byte {
	off := int(binary.BigEndian.Uint64(args[slot*32+24:]))
	n := int(binary.BigEndian.Uint64(args[off+24:]))
	head := off + 32
	out := make([][]byte, n)
	for i := range out {
		eo := head + int(binary.BigEndian.Uint64(args[head+i*32+24:]))
		l := int(binary.BigEndian.Uint64(args[eo+24:]))
		out[i] = args[eo+32 : eo+32+l]
	}
	return out
}

// fakeStepper plays KVMStepRunner.executeChunk for a program whose run emits
// one stream byte per step, writes one memory word per chunk and opens one
// path segment per chunk (clearing the path every third chunk). It keeps the
// state it handed out so it can check the client threads it back exactly.
type fakeStepper struct {
	t        *testing.T
	total    int
	gasStep  uint64
	noise    []byte // expected noise table, or nil
	mu       sync.Mutex
	expected map[string][2][]byte // snapshot -> (memory image, path blob)
}

func (f *fakeStepper) streamByte(i int) byte { return byte(i*31 + 7) }

func (f *fakeStepper) handle(data []byte) ([]byte, uint64, error) {
	args := data[4:]
	r := abi.NewRet(args)
	snap, err := r.BytesAt(3)
	if err != nil {
		return nil, 0, err
	}
	budget, err := r.U64At(5)
	if err != nil {
		return nil, 0, err
	}
	blobs := bytesArray(f.t, args, 4)
	if len(blobs) != 3 {
		return nil, 0, fmt.Errorf("want 3 blobs, got %d", len(blobs))
	}
	if f.noise != nil && !bytes.Equal(blobs[1], f.noise) {
		return nil, 0, errors.New("noise table not threaded")
	}
	cursor := 0
	var mem, path []byte
	if len(snap) > 0 {
		s, err := DecodeSnapshot(snap)
		if err != nil {
			return nil, 0, err
		}
		cursor = int(s.Steps)
		f.mu.Lock()
		want, ok := f.expected[string(snap)]
		f.mu.Unlock()
		if !ok {
			return nil, 0, errors.New("unknown snapshot")
		}
		if !bytes.Equal(blobs[0], want[0]) || !bytes.Equal(blobs[2], want[1]) {
			return nil, 0, errors.New("client-held state not threaded")
		}
		mem, path = append([]byte{}, want[0]...), append([]byte{}, want[1]...)
	}
	gas := 50_000 + f.gasStep*budget
	end := min(cursor+int(budget), f.total)
	var chunk []byte
	for i := cursor; i < end; i++ {
		chunk = append(chunk, f.streamByte(i))
	}
	done := end == f.total
	n := end / 1000 // a pure function of the inputs: measurement calls repeat calls

	// memory: bound 4 words, write word n%4 = end
	if len(mem) == 0 {
		mem = make([]byte, 32)
	}
	idx := n % 4
	memDelta := []byte{0, 4, 0, 0, 0, 1, 0, byte(idx)}
	memDelta = binary.BigEndian.AppendUint64(memDelta, uint64(end))
	binary.BigEndian.PutUint64(mem[idx*8:], uint64(end))
	// path: one M segment at (end, end); cleared every third chunk
	seg := []byte{1}
	seg = binary.BigEndian.AppendUint64(seg, uint64(end))
	seg = binary.BigEndian.AppendUint64(seg, uint64(end))
	cleared := byte(0)
	if n%3 == 0 {
		cleared = 1
		path = nil
	}
	path = append(path, seg...)
	pathDelta := append([]byte{cleared, 0, 0, 0, 1}, seg...)

	var next []byte
	if !done {
		s := &Snapshot{Flags: FlagMemory | FlagHasCurrent, Steps: uint64(end), MemBound: 4,
			PathSegs: uint32(len(path) / 17), CurX: int64(end), CurY: int64(end)}
		next = EncodeSnapshot(s)
		f.mu.Lock()
		f.expected[string(next)] = [2][]byte{mem, path}
		f.mu.Unlock()
	}
	ret := new(abi.Encoder).Bytes(chunk).Bytes(next).Bytes(memDelta).Bytes(pathDelta).Bool(done).Done()
	return ret, gas, nil
}

func fakeNoise(data []byte) ([]byte, uint64, error) {
	r := abi.NewRet(data[4:])
	g, _ := r.BigAt(0)
	count, _ := r.U64At(1)
	vs := make([]int64, count)
	for i := range vs {
		vs[i] = g.Int64() + int64(i)
	}
	next := new(big.Int).Add(g, new(big.Int).SetUint64(count))
	return new(abi.Encoder).Int64s(vs).Uint(next.Uint64()).Done(), 23_000 * count, nil
}

func expectedNoise(seed int32) []byte {
	out := make([]byte, NoiseTableBytes)
	g := int64(uint32(seed))
	for i := 0; i < 4096; i++ {
		binary.BigEndian.PutUint64(out[i*8:], uint64(g+int64(i)))
	}
	return out
}

func TestStreamThreadsStateUnderTheCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cap     uint64
		noTrace bool
		noise   bool
	}{
		{"measured", 50_000_000, false, true},
		{"unmeasured", 50_000_000, true, false},
		{"tight cap", 3_000_000, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := fakechain.New()
			defer chain.Close()
			chain.NoTrace = tc.noTrace
			f := &fakeStepper{t: t, total: 200_000, gasStep: 900, expected: map[string][2][]byte{}}
			cfg := &Config{StepRunner: stepAddr, Program: []byte("KVM1"), Seed: -5}
			if tc.noise {
				f.noise = expectedNoise(cfg.Seed)
				chain.Handle(noiseAddr, fakeNoise)
				cfg.NoiseLib = noiseAddr
			}
			chain.Handle(stepAddr, f.handle)
			meter := rpc.MeterAuto
			if tc.noTrace {
				meter = rpc.MeterTrace
			}
			cfg.Caller = rpc.NewCaller(rpc.New(chain.URL()), tc.cap, meter)
			res, err := Stream(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Caller.Wait()
			if len(res.Stream) != f.total {
				t.Fatalf("stream %d bytes, want %d", len(res.Stream), f.total)
			}
			for i, b := range res.Stream {
				if b != f.streamByte(i) {
					t.Fatalf("stream byte %d wrong", i)
				}
			}
			if tc.noise && (res.NoiseCalls < 3 || tc.cap < 10_000_000 && res.NoiseCalls < 10) {
				t.Fatalf("noise calls %d: the table was not sliced to the cap", res.NoiseCalls)
			}
			t.Logf("calls %d, retries %d, noise %d", res.Calls, res.Retries, res.NoiseCalls)
		})
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := &Snapshot{
		Flags: FlagMemory | FlagRv1 | FlagHasCurrent | FlagFlipH, PC: 77, Steps: 1 << 40, TraitCount: 3,
		Stack: []int64{-1, 2}, Locals: []int64{5}, CallStack: []uint32{9}, MemBound: 16, MemcapSet: true,
		Seed: 0xfffffffe, Rv1Seed: make([]uint32, 56), Rv1Inext: 1, Rv1Inextp: 22,
		Beziers:  []Bezier{{Slot: 2, Ax: 1, Dy: -1, Arc: []int64{4, 5}}},
		PathSegs: 2, CurX: 10, CurY: -10, FlipH: 1 << 32, GlyphEm: 3,
		Style: Style{M: [6]int64{1, 0, 0, 1, 0, 0}, HasFill: true, Fill: 0xff00ff00, ClipMode: 1,
			Grad: &Grad{Kind: 1, Stops: []GradStop{{0, 1}, {65535, 2}}}},
		Saves:  []Style{{Field: &Field{Kind: 1, Scale: 7, ColorA: 1, ColorB: 2}}},
		Noise1: &Noise1{Octaves: 4, Gain: 1 << 31},
	}
	b := EncodeSnapshot(s)
	if binary.BigEndian.Uint32(b) != MagicKSN6 {
		t.Fatalf("magic %x, want KSN6", b[:4])
	}
	d, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(EncodeSnapshot(d), b) {
		t.Fatal("encode(decode(x)) != x")
	}
	if _, err := DecodeSnapshot(append(b, 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	if _, err := DecodeSnapshot(b[:len(b)-1]); err == nil {
		t.Fatal("truncated snapshot accepted")
	}
	plain := EncodeSnapshot(&Snapshot{Steps: 5})
	if binary.BigEndian.Uint32(plain) != MagicKSN3 {
		t.Fatal("a plain snapshot must encode as KSN3")
	}
}

func TestDeltaFolds(t *testing.T) {
	img, err := ApplyMemDelta(nil, []byte{0, 2, 0, 0, 0, 1, 0, 1, 1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(img) != 16 || !bytes.Equal(img[8:], []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("image %x", img)
	}
	if _, err := ApplyMemDelta(img, []byte{0, 2, 0, 0, 0, 1, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("out-of-bound write accepted")
	}
	p, _ := ApplyPathDelta([]byte{4}, []byte{0, 0, 0, 0, 1, 4})
	if !bytes.Equal(p, []byte{4, 4}) {
		t.Fatalf("append fold %x", p)
	}
	p, _ = ApplyPathDelta(p, []byte{1, 0, 0, 0, 1, 4})
	if !bytes.Equal(p, []byte{4}) {
		t.Fatalf("cleared fold %x", p)
	}
	seg := []byte{2}
	seg = binary.BigEndian.AppendUint64(seg, 3)
	seg = binary.BigEndian.AppendUint64(seg, 4)
	x, y, has, err := WalkPathBlob(append(seg, 4), 2)
	if err != nil || !has || x != 3 || y != 4 {
		t.Fatalf("walk %d %d %v %v", x, y, has, err)
	}
	if _, _, _, err := WalkPathBlob(seg, 2); err == nil {
		t.Fatal("seg count mismatch accepted")
	}
}

func TestCheckpointResume(t *testing.T) {
	chain := fakechain.New()
	defer chain.Close()
	f := &fakeStepper{t: t, total: 300_000, gasStep: 900, expected: map[string][2][]byte{}}
	chain.Handle(stepAddr, f.handle)
	ckpt := t.TempDir() + "/run.ckpt"
	newCfg := func() *Config {
		return &Config{StepRunner: stepAddr, Program: []byte("KVM1"), Seed: 9, Checkpoint: ckpt,
			Caller: rpc.NewCaller(rpc.New(chain.URL()), 20_000_000, rpc.MeterAuto)}
	}
	// First run: interrupted after 3 chunks.
	ctx, cancel := context.WithCancel(context.Background())
	cfg := newCfg()
	cfg.OnProgress = func(p Progress) {
		if p.Calls == 3 {
			cancel()
		}
	}
	if _, err := Stream(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	cfg.Caller.Wait()
	// A different run must not pick the checkpoint up.
	other := newCfg()
	other.Seed = 10
	if _, err := Stream(context.Background(), other); err == nil {
		t.Fatal("a checkpoint for another seed was accepted")
	}
	// Second run: resumes and completes.
	cfg = newCfg()
	resumedAt := -1
	cfg.OnResume = func(calls, _ int) { resumedAt = calls }
	res, err := Stream(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Caller.Wait()
	if resumedAt != 3 || res.Resumed != 3 {
		t.Fatalf("resumed at %d (%d), want 3", resumedAt, res.Resumed)
	}
	if len(res.Stream) != f.total {
		t.Fatalf("stream %d bytes, want %d", len(res.Stream), f.total)
	}
	for i, b := range res.Stream {
		if b != f.streamByte(i) {
			t.Fatalf("stream byte %d wrong after resume", i)
		}
	}
	if _, err := LoadCheckpoint(ckpt, RunKey(stepAddr, []byte("KVM1"), 9, nil)); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatal("checkpoint not removed after completion")
	}
}

func TestCheckpointKeepDone(t *testing.T) {
	chain := fakechain.New()
	defer chain.Close()
	f := &fakeStepper{t: t, total: 50_000, gasStep: 900, expected: map[string][2][]byte{}}
	chain.Handle(stepAddr, f.handle)
	ckpt := t.TempDir() + "/run.ckpt"
	cfg := &Config{StepRunner: stepAddr, Program: []byte("KVM1"), Seed: 1, Checkpoint: ckpt, KeepDone: true,
		Caller: rpc.NewCaller(rpc.New(chain.URL()), 20_000_000, rpc.MeterOff)}
	first, err := Stream(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := chain.Calls[strings.ToLower(stepAddr)]
	again, err := Stream(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if chain.Calls[strings.ToLower(stepAddr)] != before {
		t.Fatal("a complete checkpoint still made calls")
	}
	if !bytes.Equal(first.Stream, again.Stream) || again.Resumed != first.Calls {
		t.Fatal("complete checkpoint did not return the stream")
	}
}
