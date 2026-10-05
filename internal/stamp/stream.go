// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package stamp renders a stamp-family (family 3) draw stream through the
// deployed StampRenderer, in windows and op ranges that each fit under the
// node's gas cap.
package stamp

import (
	"encoding/binary"
	"fmt"
)

// Record tags of a stamp-family draw stream, as StampRenderer reads them.
const (
	tagBegin      = 0xb0 // u16 width | u16 height
	tagEnd        = 0xb1
	tagBackground = 0x00 // u32 argb
	tagTrait      = 0x02 // u8 id | i64 value (not an op)
	tagStamp      = 0x04 // u32 color | u8 tip | u8 blend | 6 x i64 (cx cy size dirx diry pressure)
	tagField      = 0x06 // u8 kind | u32 colorA | u32 colorB [kind 1: i64 scale | i64 warp | i32 seed]
)

// Op is one drawing record of the stream, in stream order.
type Op struct {
	Tag  byte
	Kind byte // field kind (tagField only)
	Off  int  // byte offset of the record in the stream
	Len  int  // record length in bytes
	// Footprint of a stamp in y-up canvas pixels, inclusive, before window
	// clamping: exactly the box StampRenderer computes. Empty when the stamp
	// draws nothing (size <= 0).
	X0, Y0, X1, Y1 int64
	Empty          bool
	// Unbounded is set when the record's coordinates are so large that the
	// contract's saturating fixed-point math could differ from plain int64
	// math; such a stamp is never culled.
	Unbounded bool
}

// FullCanvas reports whether the op paints every pixel of any window it is
// rendered in (background and fields).
func (o *Op) FullCanvas() bool { return o.Tag == tagBackground || o.Tag == tagField }

// Stream is a parsed stamp-family draw stream.
type Stream struct {
	Raw    []byte
	Width  int
	Height int
	Ops    []Op
	Traits int
	// Counts by kind, for the report.
	Stamps, Fields, NoiseFields, Backgrounds int
}

const one = int64(1) << 32

// Parse walks a draw stream exactly as StampRenderer does and rejects any
// record it would reject.
func Parse(b []byte) (*Stream, error) {
	if len(b) < 6 || b[0] != tagBegin {
		return nil, fmt.Errorf("stamp: not a draw stream (want 0xb0 header)")
	}
	s := &Stream{Raw: b, Width: int(binary.BigEndian.Uint16(b[1:])), Height: int(binary.BigEndian.Uint16(b[3:]))}
	hOne := int64(s.Height) * one
	off := 5
	for {
		if off >= len(b) {
			return nil, fmt.Errorf("stamp: stream ends without an end record")
		}
		tag := b[off]
		need := func(n int) error {
			if off+n > len(b) {
				return fmt.Errorf("stamp: record 0x%02x at %d truncated", tag, off)
			}
			return nil
		}
		switch tag {
		case tagEnd:
			if off+1 != len(b) {
				return nil, fmt.Errorf("stamp: %d trailing bytes after the end record", len(b)-off-1)
			}
			return s, nil
		case tagBackground:
			if err := need(5); err != nil {
				return nil, err
			}
			s.Ops = append(s.Ops, Op{Tag: tag, Off: off, Len: 5})
			s.Backgrounds++
			off += 5
		case tagTrait:
			if err := need(10); err != nil {
				return nil, err
			}
			s.Traits++
			off += 10
		case tagStamp:
			if err := need(55); err != nil {
				return nil, err
			}
			op := Op{Tag: tag, Off: off, Len: 55}
			i64 := func(o int) int64 { return int64(binary.BigEndian.Uint64(b[off+o:])) }
			r := i64(23)
			const lim = int64(1) << 60
			cx, cy := i64(7), i64(15)
			if r <= 0 {
				op.Empty = true
			} else if r > lim || cx > lim || cx < -lim || cy > lim || cy < -lim {
				op.Unbounded = true
			} else {
				sxc := cx
				syc := hOne - cy
				ext := r + one
				op.X0, op.X1 = (sxc-ext)>>32, (sxc+ext)>>32
				op.Y0, op.Y1 = (syc-ext)>>32, (syc+ext)>>32
			}
			s.Ops = append(s.Ops, op)
			s.Stamps++
			off += 55
		case tagField:
			if err := need(2); err != nil {
				return nil, err
			}
			kind := b[off+1]
			n := 10
			if kind == 1 {
				n = 30
				s.NoiseFields++
			}
			if err := need(n); err != nil {
				return nil, err
			}
			s.Ops = append(s.Ops, Op{Tag: tag, Kind: kind, Off: off, Len: n})
			s.Fields++
			off += n
		default:
			return nil, fmt.Errorf("stamp: unknown record 0x%02x at %d", tag, off)
		}
	}
}

// Rect is a window in y-up canvas pixels.
type Rect struct{ X0, Y0, W, H int }

// Touches reports whether the op can write a pixel of the window. Background
// and fields paint every window. A stamp writes only inside its footprint
// box, so a stamp whose box misses the window leaves it untouched and can be
// left out of that window's calls without changing a pixel.
func (o *Op) Touches(w Rect) bool {
	if o.FullCanvas() || o.Unbounded {
		return true
	}
	if o.Empty {
		return false
	}
	return o.X1 >= int64(w.X0) && o.X0 <= int64(w.X0+w.W-1) &&
		o.Y1 >= int64(w.Y0) && o.Y0 <= int64(w.Y0+w.H-1)
}

// SubStream builds a valid draw stream holding only the given ops, in the
// given order, with the original header. Every stamp-family record is
// self-describing, so a sub-stream renders those records exactly as the full
// stream would.
func (s *Stream) SubStream(ops []int) []byte {
	n := 6
	for _, i := range ops {
		n += s.Ops[i].Len
	}
	out := make([]byte, 0, n)
	out = append(out, s.Raw[:5]...)
	for _, i := range ops {
		o := s.Ops[i]
		out = append(out, s.Raw[o.Off:o.Off+o.Len]...)
	}
	return append(out, tagEnd)
}
