// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package stamp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/fakechain"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
)

// buildStream makes a stamp-family stream: a background, a trait, then n
// stamps at random positions and sizes (a few huge, a few empty).
func buildStream(w, h, n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	b := []byte{tagBegin, byte(w >> 8), byte(w), byte(h >> 8), byte(h)}
	b = append(b, tagBackground, 0xff, 0x10, 0x20, 0x30)
	b = append(b, tagTrait, 7)
	b = binary.BigEndian.AppendUint64(b, 42)
	for i := 0; i < n; i++ {
		r := rng.Int63n(12) << 32
		switch i % 50 {
		case 7:
			r = 0 // draws nothing
		case 11:
			r = int64(h) << 32 // covers the whole canvas
		}
		b = append(b, tagStamp)
		b = binary.BigEndian.AppendUint32(b, rng.Uint32())
		b = append(b, byte(rng.Intn(4)), byte(rng.Intn(3)))
		for _, v := range []int64{rng.Int63n(int64(w)+20)<<32 - 10<<32, rng.Int63n(int64(h)+20)<<32 - 10<<32, r, 0, 0, 1 << 32} {
			b = binary.BigEndian.AppendUint64(b, uint64(v))
		}
	}
	return append(b, tagEnd)
}

// paint is the fake rasterizer: background fills the window; a stamp mixes
// its color into every pixel of its clamped footprint with an order-dependent
// rule, so any reordering, dropped op or wrong window shows up in the pixels.
func paint(buf []byte, s *Stream, win Rect) uint64 {
	gas := uint64(0)
	for i := range s.Ops {
		o := &s.Ops[i]
		rec := s.Raw[o.Off:]
		switch o.Tag {
		case tagBackground:
			for p := 0; p < len(buf); p += 4 {
				copy(buf[p:], []byte{rec[4], rec[3], rec[2], rec[1]})
			}
			gas += uint64(len(buf))
		case tagStamp:
			gas += 2000
			if o.Empty {
				continue
			}
			x0, x1 := max(o.X0, int64(win.X0)), min(o.X1, int64(win.X0+win.W-1))
			y0, y1 := max(o.Y0, int64(win.Y0)), min(o.Y1, int64(win.Y0+win.H-1))
			for y := y0; y <= y1; y++ {
				for x := x0; x <= x1; x++ {
					p := ((int(y)-win.Y0)*win.W + int(x) - win.X0) * 4
					for ch := 0; ch < 4; ch++ {
						buf[p+ch] = buf[p+ch]*7 + rec[1+ch] + rec[9+ch]
					}
					gas += 300
				}
			}
		}
	}
	return gas
}

// fakeRenderer plays StampRenderer.renderTileP with a gas model: a fixed cost,
// a cost per byte of calldata, and the paint cost.
func fakeRenderer(t *testing.T) fakechain.Handler {
	return func(data []byte) ([]byte, uint64, error) {
		r := abi.NewRet(data[4:])
		raw, err := r.BytesAt(0)
		if err != nil {
			return nil, 0, err
		}
		var win [4]int64
		for i := range win {
			if win[i], err = r.I64At(1 + i); err != nil {
				return nil, 0, err
			}
		}
		prior, err := r.BytesAt(7)
		if err != nil {
			return nil, 0, err
		}
		s, err := Parse(raw)
		if err != nil {
			return nil, 0, err
		}
		rect := Rect{X0: int(win[0]), Y0: int(win[1]), W: int(win[2]), H: int(win[3])}
		buf := make([]byte, rect.W*rect.H*4)
		if len(prior) != 0 {
			if len(prior) != len(buf) {
				return nil, 0, errors.New("bad prior")
			}
			copy(buf, prior)
		}
		gas := 21000 + uint64(len(data))*16 + paint(buf, s, rect)
		return new(abi.Encoder).Bytes(buf).Uint(uint64(len(s.Ops))).Done(), gas, nil
	}
}

func oneShot(t *testing.T, raw []byte) []byte {
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	win := Rect{0, 0, s.Width, s.Height}
	buf := make([]byte, s.Width*s.Height*4)
	paint(buf, s, win)
	return bgraToRGBA(buf, s.Width, s.Height)
}

const rendererAddr = "0x454ead37E6254101f9abe416a4Ff25CAF75cbaa1"

func TestBandedRenderMatchesOneShot(t *testing.T) {
	raw := buildStream(97, 61, 400, 1)
	want := oneShot(t, raw)
	for _, tc := range []struct {
		name    string
		cap     uint64
		band    int
		workers int
		noTrace bool
	}{
		{"roomy", 50_000_000, 16, 3, false},
		{"tight cap forces small slices and splits", 2_000_000, 32, 2, false},
		{"no gas measurement", 3_000_000, 8, 4, true},
		{"one band", 50_000_000, 64, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := fakechain.New()
			defer chain.Close()
			chain.NoTrace = tc.noTrace
			chain.Handle(rendererAddr, fakeRenderer(t))
			meter := rpc.MeterAuto
			if tc.noTrace {
				meter = rpc.MeterTrace
			}
			caller := rpc.NewCaller(rpc.New(chain.URL()), tc.cap, meter)
			s, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			res, err := Render(context.Background(), &Config{Caller: caller, Renderer: rendererAddr, BandH: tc.band, Workers: tc.workers}, s)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(res.RGBA, want) {
				t.Fatal("banded render differs from the one-shot render")
			}
			t.Logf("calls %d, splits %d, misses %d", res.Calls, res.Splits, caller.Stats().Misses)
		})
	}
}

func TestBandSplitsWhenOneOpDoesNotFit(t *testing.T) {
	raw := buildStream(64, 64, 60, 2) // op 11 covers the whole canvas
	want := oneShot(t, raw)
	chain := fakechain.New()
	defer chain.Close()
	chain.Handle(rendererAddr, fakeRenderer(t))
	// The whole-canvas stamp needs about 1.2M gas in a 64-row band; 1M forces
	// the band to split, and 32 rows fit.
	caller := rpc.NewCaller(rpc.New(chain.URL()), 1_000_000, rpc.MeterAuto)
	s, _ := Parse(raw)
	res, err := Render(context.Background(), &Config{Caller: caller, Renderer: rendererAddr, BandH: 64, Workers: 1}, s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Splits == 0 {
		t.Fatal("expected the band to split")
	}
	if !bytes.Equal(res.RGBA, want) {
		t.Fatal("split render differs from the one-shot render")
	}
	// Far too small for anything: the error says so instead of looping.
	caller = rpc.NewCaller(rpc.New(chain.URL()), 30_000, rpc.MeterAuto)
	if _, err := Render(context.Background(), &Config{Caller: caller, Renderer: rendererAddr, BandH: 64, Workers: 1}, s); !errors.Is(err, errIrreducible) {
		t.Fatalf("want errIrreducible, got %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	good := buildStream(10, 10, 3, 3)
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"no header":      good[1:],
		"no end":         good[:len(good)-1],
		"trailing bytes": append(append([]byte{}, good...), 0),
		"unknown record": append(append([]byte{}, good[:5]...), 0x09, tagEnd),
		"truncated":      append(append([]byte{}, good[:5]...), tagStamp, 1, 2),
	} {
		if _, err := Parse(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFootprintMatchesContract(t *testing.T) {
	// cx = 10.5, cy = 20.25, size = 3 on a 100-high canvas:
	// syc = 100 - 20.25 = 79.75; ext = 4
	b := []byte{tagBegin, 0, 100, 0, 100, tagStamp, 0, 0, 0, 0, 0, 0}
	for _, v := range []int64{21 << 31, 81 << 30, 3 << 32, 0, 0, 0} {
		b = binary.BigEndian.AppendUint64(b, uint64(v))
	}
	b = append(b, tagEnd)
	s, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	o := s.Ops[0]
	if o.X0 != 6 || o.X1 != 14 || o.Y0 != 75 || o.Y1 != 83 {
		t.Fatalf("footprint (%d,%d)-(%d,%d)", o.X0, o.Y0, o.X1, o.Y1)
	}
	if !o.Touches(Rect{14, 83, 5, 5}) || o.Touches(Rect{15, 0, 5, 100}) || o.Touches(Rect{0, 84, 100, 5}) {
		t.Fatal("Touches disagrees with the footprint")
	}
}

func TestSubStreamKeepsOrder(t *testing.T) {
	raw := buildStream(20, 20, 10, 4)
	s, _ := Parse(raw)
	sub := s.SubStream([]int{0, 3, 7})
	t2, err := Parse(sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(t2.Ops) != 3 || t2.Traits != 0 {
		t.Fatalf("sub-stream has %d ops, %d traits", len(t2.Ops), t2.Traits)
	}
	for i, j := range []int{0, 3, 7} {
		a, b := s.Ops[j], t2.Ops[i]
		if !bytes.Equal(s.Raw[a.Off:a.Off+a.Len], t2.Raw[b.Off:b.Off+b.Len]) {
			t.Fatalf("op %d changed", j)
		}
	}
}
