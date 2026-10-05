// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package stamp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
)

// Config describes one full-frame render through StampRenderer.renderTileP.
//
// The frame is cut into full-width horizontal bands. Each band is one window
// of renderTileP. A band's ops (background, fields, and every stamp whose
// footprint touches the band) are sent in slices, in stream order, and each
// call threads the band's pixel buffer from the previous call back in as
// `prior`. StampRenderer composites every record independently, so this gives
// exactly the pixels of a one-shot render, for any band height and any slice
// boundaries.
type Config struct {
	Caller   *rpc.Caller
	Renderer string // StampRenderer address
	BandH    int    // band height in pixels (default 64)
	Workers  int    // bands rendered concurrently (default 4)
	// MaxOps and MaxSliceBytes bound one call's op slice (defaults 20,000 ops
	// and 600,000 bytes of records); raise them for a node without a gas cap.
	MaxOps        int
	MaxSliceBytes int
	// OnProgress is called after every successful call (from worker goroutines).
	OnProgress func(Progress)
}

// Progress is live telemetry for the terminal.
type Progress struct {
	BandsDone, Bands int
	OpsDone, OpsAll  int64
	Calls            int64
}

// Result is the composited frame plus telemetry.
type Result struct {
	RGBA   []byte // top-down RGBA, Width*Height*4
	Bands  int
	Splits int // bands re-rendered at half height because a single op did not fit
	Calls  int64
}

type band struct{ y0, h int } // device rows, top-down

const (
	maxSlice    = 20_000
	maxSliceB   = 600_000 // calldata budget for the op slice, in bytes
	minBandH    = 4
	targetShare = 0.88 // aim each call at this share of the cap
)

// Render rasterizes the stream through the deployed StampRenderer.
func Render(ctx context.Context, cfg *Config, s *Stream) (*Result, error) {
	bandH := cfg.BandH
	if bandH <= 0 {
		bandH = 64
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 4
	}
	var bands []band
	for y := 0; y < s.Height; y += bandH {
		bands = append(bands, band{y0: y, h: min(bandH, s.Height-y)})
	}
	res := &Result{RGBA: make([]byte, s.Width*s.Height*4), Bands: len(bands)}

	var opsAll int64
	for _, b := range bands {
		opsAll += int64(len(s.opsFor(s.window(b))))
	}
	var (
		opsDone, calls, bandsDone, splits atomic.Int64
		firstErr                          error
		errOnce                           sync.Once
		wg                                sync.WaitGroup
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan band)
	r := &renderer{cfg: cfg, s: s, opsDone: &opsDone, calls: &calls, splits: &splits,
		progress: func() {
			if cfg.OnProgress != nil {
				cfg.OnProgress(Progress{BandsDone: int(bandsDone.Load()), Bands: len(bands),
					OpsDone: opsDone.Load(), OpsAll: opsAll, Calls: calls.Load()})
			}
		}}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range work {
				rgba, err := r.band(ctx, b)
				if err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("band y %d..%d: %w", b.y0, b.y0+b.h, err)
						cancel()
					})
					continue
				}
				copy(res.RGBA[b.y0*s.Width*4:], rgba)
				bandsDone.Add(1)
				r.progress()
			}
		}()
	}
feed:
	for _, b := range bands {
		select {
		case work <- b:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res.Calls = calls.Load()
	res.Splits = int(splits.Load())
	return res, nil
}

// window converts a top-down band to the y-up window renderTileP takes.
func (s *Stream) window(b band) Rect {
	return Rect{X0: 0, Y0: s.Height - b.y0 - b.h, W: s.Width, H: b.h}
}

// opsFor lists, in stream order, the ops that can write a pixel of w.
func (s *Stream) opsFor(w Rect) []int {
	var out []int
	for i := range s.Ops {
		if s.Ops[i].Touches(w) {
			out = append(out, i)
		}
	}
	return out
}

type renderer struct {
	mu sync.Mutex
	g  float64 // gas per weight unit, shared so later bands start calibrated

	measured atomic.Bool // some call has been measured

	cfg                    *Config
	s                      *Stream
	opsDone, calls, splits *atomic.Int64
	progress               func()
}

// errIrreducible marks a single op that does not fit the cap in this window:
// the band is split in half and rendered again.
var errIrreducible = errors.New("a single op does not fit the gas cap in this window")

// band renders one band and returns its top-down RGBA rows. When one op alone
// does not fit, it renders the band again as two half-height bands. Window
// geometry does not change pixels, so the split is exact.
func (r *renderer) band(ctx context.Context, b band) ([]byte, error) {
	rgba, err := r.window(ctx, b)
	if err == nil || !errors.Is(err, errIrreducible) || b.h/2 < minBandH {
		return rgba, err
	}
	r.splits.Add(1)
	h1 := b.h / 2
	top, err := r.band(ctx, band{y0: b.y0, h: h1})
	if err != nil {
		return nil, err
	}
	bot, err := r.band(ctx, band{y0: b.y0 + h1, h: b.h - h1})
	if err != nil {
		return nil, err
	}
	return append(top, bot...), nil
}

// The slicer predicts a call's gas as base + g*W, where base is the cost of
// the calldata and memory the call carries (mostly the threaded prior), W is
// the summed work weight of the slice's ops (pixels each op touches inside
// the window, plus a per-op overhead), and g is gas per unit of weight,
// learned from the measured gas of earlier calls in the band.
const (
	opOverhead = 40     // per-op work in pixel units (record parse, setup)
	gInitial   = 1200.0 // gas per pixel before anything is measured
)

// weight is the work an op does in window w, in pixel units.
func (s *Stream) weight(o *Op, w Rect) float64 {
	if o.FullCanvas() {
		if o.Tag == tagField && o.Kind == 1 {
			return float64(w.W*w.H) * 40 // noise ground: three noise samples per pixel
		}
		return float64(w.W*w.H)/8 + opOverhead
	}
	if o.Empty || o.Unbounded {
		return opOverhead
	}
	x0, x1 := max(o.X0, int64(w.X0)), min(o.X1, int64(w.X0+w.W-1))
	y0, y1 := max(o.Y0, int64(w.Y0)), min(o.Y1, int64(w.Y0+w.H-1))
	if x0 > x1 || y0 > y1 {
		return opOverhead
	}
	return float64((x1-x0+1)*(y1-y0+1)) + opOverhead
}

// baseGas estimates the gas of a renderTileP call before any op work: the
// intrinsic cost, calldata, and memory expansion over the prior, the window
// buffer and the returned copy.
func baseGas(sliceBytes, priorBytes, windowBytes int) float64 {
	calldata := 16 * float64(sliceBytes+priorBytes+400)
	words := float64(priorBytes+3*windowBytes+2*sliceBytes) / 32
	return 21000 + calldata + 3*words + words*words/512
}

func (r *renderer) window(ctx context.Context, b band) ([]byte, error) {
	s := r.s
	w := r.s.window(b)
	ops := s.opsFor(w)
	weights := make([]float64, len(ops))
	for n, i := range ops {
		weights[n] = s.weight(&s.Ops[i], w)
	}
	win := abi.StampWindow{X0: int64(w.X0), Y0: int64(w.Y0), W: int64(w.W), H: int64(w.H), OpStart: 0, OpCount: ^uint64(0) >> 1}
	target := float64(r.cfg.Caller.Cap) * targetShare
	windowBytes := w.W * w.H * 4
	r.mu.Lock()
	g := r.g
	r.mu.Unlock()
	if g == 0 {
		g = gInitial
	}
	defer func() {
		r.mu.Lock()
		r.g = g
		r.mu.Unlock()
	}()
	maxOps, maxBytes := maxSlice, maxSliceB
	if r.cfg.MaxOps > 0 {
		maxOps = r.cfg.MaxOps
	}
	if r.cfg.MaxSliceBytes > 0 {
		maxBytes = r.cfg.MaxSliceBytes
	}
	var prior []byte
	limit := len(ops) // cap on the slice length after a miss at this position
	for i := 0; i < len(ops) || prior == nil; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Grow the slice while the predicted gas stays under the target.
		kk, wsum := 0, 0.0
		for i+kk < len(ops) && kk < limit && kk < maxOps && (kk+1)*55 <= maxBytes {
			next := wsum + weights[i+kk]
			if kk > 0 && baseGas((kk+1)*55, len(prior), windowBytes)+g*next > target {
				break
			}
			wsum = next
			kk++
		}
		kk = max(kk, min(1, len(ops)-i))
		sub := s.SubStream(ops[i : i+kk])
		ret, gas, err := r.cfg.Caller.CallMeasured(ctx, r.cfg.Renderer, abi.EncodeRenderTileP(sub, win, prior))
		if err != nil {
			var rev *rpc.RevertError
			if !errors.As(err, &rev) {
				return nil, err
			}
			if kk <= 1 {
				return nil, fmt.Errorf("%w (op %d): %v", errIrreducible, ops[i], rev)
			}
			// Did not fit: the model was optimistic. Raise g and halve the
			// slice; retry from the same prior.
			g *= 1.25
			limit = max(1, kk/2)
			continue
		}
		buf, total, err := abi.DecodeRenderTileP(ret)
		if err != nil {
			return nil, err
		}
		if total != uint64(kk) {
			return nil, fmt.Errorf("renderer counted %d ops in a slice of %d", total, kk)
		}
		if len(buf) != windowBytes {
			return nil, fmt.Errorf("renderer returned %d bytes for a %dx%d window", len(buf), w.W, w.H)
		}
		base := baseGas(kk*55, len(prior), windowBytes)
		prior = buf
		i += kk
		limit = len(ops)
		r.calls.Add(1)
		r.opsDone.Add(int64(kk))
		r.progress()
		if gas > 0 && wsum > 0 {
			r.measured.Store(true)
			obs := (float64(gas) - base) / wsum
			if obs > 0 {
				g = 0.5*g + 0.5*obs
			}
		} else if !r.measured.Load() {
			g *= 0.98 // nothing measured at all: creep toward bigger slices
		}
	}
	return bgraToRGBA(prior, w.W, w.H), nil
}

// bgraToRGBA converts a y-up BGRA window buffer (StampRenderer's layout) to
// top-down RGBA rows, as StampRenderer.tileToRGBA does on chain.
func bgraToRGBA(buf []byte, w, h int) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		src := (h - 1 - y) * w * 4
		dst := y * w * 4
		for x := 0; x < w; x++ {
			out[dst+x*4+0] = buf[src+x*4+2]
			out[dst+x*4+1] = buf[src+x*4+1]
			out[dst+x*4+2] = buf[src+x*4+0]
			out[dst+x*4+3] = buf[src+x*4+3]
		}
	}
	return out
}
