// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Snapshot codec for the stepped interpreter's resume state.
//
// KVMStepRunner.executeChunk returns a snapshot after every chunk; the next
// call resumes from it. The tool treats the snapshot as opaque resume state,
// and decodes it only to VALIDATE what the chain handed back (structure, the
// segment count against the client-held path blob, the current-point
// invariant, the memory geometry) before trusting it for the next call.
//
// The base layout has magic "KSN3". Three variants extend it: "KSN4" (noise
// kind 1 armed: a 5-byte u8 octaves | u32 gain section right after the flags
// byte; the kind-0 table flag bit3 is rejected there), "KSN5" (a field fill
// is armed: each style carries a 25-byte {kind, scale, warp, colorA, colorB}
// sub-record after the gradient sub-record) and "KSN6" (both).
//
// Wire layout (all integers big-endian):
//
//	magic u32 | flags u8 | pc u32 | steps u64 | traitCount u32
//	stack  u16 n + n x i64
//	locals u16 n + n x i64
//	calls  u16 n + n x u32
//	memBound u16 | u8 memcapSet     (bit1: memory content is client-held)
//	seed u32
//	rv1 (if bit2): 56 x u32 | u8 inext | u8 inextp
//	beziers: u8 setCount + setCount x (u8 slot | 8 x i64 | u16 arcCount + arc x i64)
//	path u32 segCount               (segment bytes are the client-held path blob)
//	(curX i64 | curY i64 if bit6)
//	(flipH i64 if bit4) | glyphEm i64 | glyphOx i64 | glyphOy i64
//	style (current) | saves u8 n + n x style
//	[bit5 trailing: u8 0x76 | i64 i,x,y,w,h | u32 c1,c2 | i64 lineWidth]
//
// flags: bit0 halted | bit1 memory | bit2 rv1 | bit3 noiseTable | bit4 flipH
//
//	| bit5 mid-op GRADIENT cursor | bit6 hasCurrent | bit7 glyphXfActive
package stepkvm

import (
	"encoding/binary"
	"fmt"
)

const MagicKSN3 = 0x4b534e33 // "KSN3"
const MagicKSN4 = 0x4b534e34 // "KSN4" (NOISEINIT kind 1 armed)
const MagicKSN5 = 0x4b534e35 // "KSN5" (FFILL field fill armed)
const MagicKSN6 = 0x4b534e36 // "KSN6" (field fill AND kind-1 noise armed)

const (
	FlagHalted     = 1 << 0
	FlagMemory     = 1 << 1
	FlagRv1        = 1 << 2
	FlagNoiseTable = 1 << 3
	FlagFlipH      = 1 << 4
	FlagGradCursor = 1 << 5
	FlagHasCurrent = 1 << 6
	FlagGlyphXf    = 1 << 7
)

type GradStop struct {
	Off   uint16
	Color uint32
}

type Grad struct {
	Kind           byte
	X0, Y0, X1, Y1 int64
	Stops          []GradStop
}

// Field is the armed FFILL field-fill state: the KSN5/KSN6 extended-style
// sub-record. The run seed is NOT stored (it is a run input re-applied at paint
// time), so this is the 25-byte {kind, scale, warp, colorA, colorB} operand set.
type Field struct {
	Kind        byte
	Scale, Warp int64
	ColorA      uint32
	ColorB      uint32
}

type Style struct {
	M         [6]int64
	HasFill   bool
	Fill      uint32
	HasStroke bool
	Stroke    uint32
	StrokeW   int64
	ClipMode  byte // 0 none · 1 CLIPRECT · 2 CLIPRECTT (trunc)
	Clip      [8]int64
	EvenOdd   bool
	Brush     byte
	Blend     byte
	Cap       byte
	Join      byte
	Grad      *Grad
	Field     *Field // FFILL field fill; non-nil only on KSN5/KSN6 wires
}

type Bezier struct {
	Slot                           byte
	Ax, Ay, Bx, By, Cx, Cy, Dx, Dy int64
	Arc                            []int64
}

// Noise1 is the armed NOISEINIT kind-1 (GradNoise) state: the KSN4
// post-flags section. Octaves in [1,8]; Gain is the raw Fix64 in (0, ONE),
// which a u32 holds exactly (gain != 0 is the whole upper-bound check).
type Noise1 struct {
	Octaves byte
	Gain    uint32
}

// GradCursor is the parked mid-op GRADIENT checkpoint (bit5, op tag 0x76).
type GradCursor struct {
	I, X, Y, W, H int64
	C1, C2        uint32
	LineWidth     int64
}

// Snapshot is the KSN3 wire state, field-for-field.
type Snapshot struct {
	Flags      byte
	PC         uint32
	Steps      uint64
	TraitCount uint32
	Stack      []int64
	Locals     []int64
	CallStack  []uint32
	MemBound   uint16
	MemcapSet  bool
	Seed       uint32   // int32 two's complement
	Rv1Seed    []uint32 // 56 entries when FlagRv1
	Rv1Inext   byte
	Rv1Inextp  byte
	Beziers    []Bezier
	PathSegs   uint32 // seg count; seg bytes are the client-held path blob
	CurX, CurY int64  // present when FlagHasCurrent
	FlipH      int64  // present when FlagFlipH
	GlyphEm    int64
	GlyphOx    int64
	GlyphOy    int64
	Style      Style
	Saves      []Style
	GradCursor *GradCursor // present when FlagGradCursor
	Noise1     *Noise1     // non-nil == the KSN4 wire (kind-1 armed)
}

func (s *Snapshot) Halted() bool     { return s.Flags&FlagHalted != 0 }
func (s *Snapshot) HasMemory() bool  { return s.Flags&FlagMemory != 0 }
func (s *Snapshot) HasCurrent() bool { return s.Flags&FlagHasCurrent != 0 }

type reader struct {
	b   []byte
	off int
}

func (r *reader) need(n int) error {
	if r.off+n > len(r.b) {
		return fmt.Errorf("snapshot: truncated at %d (+%d > %d)", r.off, n, len(r.b))
	}
	return nil
}

func (r *reader) u8() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	v := r.b[r.off]
	r.off++
	return v, nil
}

func (r *reader) u16() (uint16, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint16(r.b[r.off:])
	r.off += 2
	return v, nil
}

func (r *reader) u32() (uint32, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v, nil
}

func (r *reader) u64() (uint64, error) {
	if err := r.need(8); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint64(r.b[r.off:])
	r.off += 8
	return v, nil
}

func (r *reader) i64() (int64, error) {
	v, err := r.u64()
	return int64(v), err
}

func readStyle(r *reader, withField bool) (Style, error) {
	var s Style
	var err error
	for i := 0; i < 6; i++ {
		if s.M[i], err = r.i64(); err != nil {
			return s, err
		}
	}
	hf, err := r.u8()
	if err != nil {
		return s, err
	}
	s.HasFill = hf != 0
	if s.Fill, err = r.u32(); err != nil {
		return s, err
	}
	hs, err := r.u8()
	if err != nil {
		return s, err
	}
	s.HasStroke = hs != 0
	if s.Stroke, err = r.u32(); err != nil {
		return s, err
	}
	if s.StrokeW, err = r.i64(); err != nil {
		return s, err
	}
	if s.ClipMode, err = r.u8(); err != nil {
		return s, err
	}
	if s.ClipMode > 2 {
		return s, fmt.Errorf("snapshot: bad clip mode %d", s.ClipMode)
	}
	if s.ClipMode != 0 {
		for i := 0; i < 8; i++ {
			if s.Clip[i], err = r.i64(); err != nil {
				return s, err
			}
		}
	}
	eo, err := r.u8()
	if err != nil {
		return s, err
	}
	s.EvenOdd = eo != 0
	if s.Brush, err = r.u8(); err != nil {
		return s, err
	}
	if s.Blend, err = r.u8(); err != nil {
		return s, err
	}
	if s.Cap, err = r.u8(); err != nil {
		return s, err
	}
	if s.Join, err = r.u8(); err != nil {
		return s, err
	}
	hg, err := r.u8()
	if err != nil {
		return s, err
	}
	if hg != 0 {
		g := &Grad{}
		if g.Kind, err = r.u8(); err != nil {
			return s, err
		}
		if g.X0, err = r.i64(); err != nil {
			return s, err
		}
		if g.Y0, err = r.i64(); err != nil {
			return s, err
		}
		if g.X1, err = r.i64(); err != nil {
			return s, err
		}
		if g.Y1, err = r.i64(); err != nil {
			return s, err
		}
		n, err := r.u8()
		if err != nil {
			return s, err
		}
		g.Stops = make([]GradStop, n)
		for i := range g.Stops {
			if g.Stops[i].Off, err = r.u16(); err != nil {
				return s, err
			}
			if g.Stops[i].Color, err = r.u32(); err != nil {
				return s, err
			}
		}
		s.Grad = g
	}
	// FFILL field-fill sub-record (KSN5/KSN6 only).
	if withField {
		hf2, err := r.u8()
		if err != nil {
			return s, err
		}
		if hf2 != 0 {
			f := &Field{}
			if f.Kind, err = r.u8(); err != nil {
				return s, err
			}
			if f.Kind > 1 {
				return s, fmt.Errorf("snapshot: FFILL kind %d out of range", f.Kind)
			}
			if f.Scale, err = r.i64(); err != nil {
				return s, err
			}
			if f.Warp, err = r.i64(); err != nil {
				return s, err
			}
			if f.ColorA, err = r.u32(); err != nil {
				return s, err
			}
			if f.ColorB, err = r.u32(); err != nil {
				return s, err
			}
			s.Field = f
		}
	}
	return s, nil
}

// DecodeSnapshot parses a snapshot, validating its structure (magic,
// halted/mid-op exclusivity, clip modes, no trailing bytes).
func DecodeSnapshot(b []byte) (*Snapshot, error) {
	r := &reader{b: b}
	magic, err := r.u32()
	if err != nil {
		return nil, err
	}
	if magic != MagicKSN3 && magic != MagicKSN4 && magic != MagicKSN5 && magic != MagicKSN6 {
		return nil, fmt.Errorf("snapshot: bad magic 0x%08x", magic)
	}
	// KSN4/KSN6 carry the kind-1 noise section; KSN5/KSN6 carry field fill in
	// the extended styles. The two extensions are orthogonal (2x2 magic ladder).
	hasNoise1 := magic == MagicKSN4 || magic == MagicKSN6
	hasField := magic == MagicKSN5 || magic == MagicKSN6
	s := &Snapshot{}
	if s.Flags, err = r.u8(); err != nil {
		return nil, err
	}
	if s.Flags&FlagGradCursor != 0 && s.Flags&FlagHalted != 0 {
		return nil, fmt.Errorf("snapshot: halted with a mid-op cursor")
	}
	if hasNoise1 {
		// the kind-1 section (u8 octaves | u32 gain) directly after flags;
		// bit3 (the kind-0 external table) cannot coexist with an armed
		// kind 1, and the params re-validate the interpreter's law.
		if s.Flags&FlagNoiseTable != 0 {
			return nil, fmt.Errorf("snapshot: kind-1 wire with bit3")
		}
		n1 := &Noise1{}
		if n1.Octaves, err = r.u8(); err != nil {
			return nil, err
		}
		if n1.Gain, err = r.u32(); err != nil {
			return nil, err
		}
		if n1.Octaves < 1 || n1.Octaves > 8 {
			return nil, fmt.Errorf("snapshot: NOISEINIT octaves %d out of range", n1.Octaves)
		}
		if n1.Gain == 0 {
			return nil, fmt.Errorf("snapshot: NOISEINIT gain out of range")
		}
		s.Noise1 = n1
	}
	if s.PC, err = r.u32(); err != nil {
		return nil, err
	}
	if s.Steps, err = r.u64(); err != nil {
		return nil, err
	}
	if s.TraitCount, err = r.u32(); err != nil {
		return nil, err
	}
	nStack, err := r.u16()
	if err != nil {
		return nil, err
	}
	s.Stack = make([]int64, nStack)
	for i := range s.Stack {
		if s.Stack[i], err = r.i64(); err != nil {
			return nil, err
		}
	}
	nLocals, err := r.u16()
	if err != nil {
		return nil, err
	}
	s.Locals = make([]int64, nLocals)
	for i := range s.Locals {
		if s.Locals[i], err = r.i64(); err != nil {
			return nil, err
		}
	}
	nCalls, err := r.u16()
	if err != nil {
		return nil, err
	}
	s.CallStack = make([]uint32, nCalls)
	for i := range s.CallStack {
		if s.CallStack[i], err = r.u32(); err != nil {
			return nil, err
		}
	}
	if s.MemBound, err = r.u16(); err != nil {
		return nil, err
	}
	mc, err := r.u8()
	if err != nil {
		return nil, err
	}
	s.MemcapSet = mc != 0
	if s.Seed, err = r.u32(); err != nil {
		return nil, err
	}
	if s.Flags&FlagRv1 != 0 {
		s.Rv1Seed = make([]uint32, 56)
		for i := range s.Rv1Seed {
			if s.Rv1Seed[i], err = r.u32(); err != nil {
				return nil, err
			}
		}
		if s.Rv1Inext, err = r.u8(); err != nil {
			return nil, err
		}
		if s.Rv1Inextp, err = r.u8(); err != nil {
			return nil, err
		}
	}
	nBez, err := r.u8()
	if err != nil {
		return nil, err
	}
	s.Beziers = make([]Bezier, nBez)
	for i := range s.Beziers {
		bz := &s.Beziers[i]
		if bz.Slot, err = r.u8(); err != nil {
			return nil, err
		}
		for _, p := range []*int64{&bz.Ax, &bz.Ay, &bz.Bx, &bz.By, &bz.Cx, &bz.Cy, &bz.Dx, &bz.Dy} {
			if *p, err = r.i64(); err != nil {
				return nil, err
			}
		}
		nArc, err := r.u16()
		if err != nil {
			return nil, err
		}
		bz.Arc = make([]int64, nArc)
		for k := range bz.Arc {
			if bz.Arc[k], err = r.i64(); err != nil {
				return nil, err
			}
		}
	}
	if s.PathSegs, err = r.u32(); err != nil {
		return nil, err
	}
	if s.Flags&FlagHasCurrent != 0 {
		if s.CurX, err = r.i64(); err != nil {
			return nil, err
		}
		if s.CurY, err = r.i64(); err != nil {
			return nil, err
		}
	}
	if s.Flags&FlagFlipH != 0 {
		if s.FlipH, err = r.i64(); err != nil {
			return nil, err
		}
	}
	if s.GlyphEm, err = r.i64(); err != nil {
		return nil, err
	}
	if s.GlyphOx, err = r.i64(); err != nil {
		return nil, err
	}
	if s.GlyphOy, err = r.i64(); err != nil {
		return nil, err
	}
	if s.Style, err = readStyle(r, hasField); err != nil {
		return nil, err
	}
	nSaves, err := r.u8()
	if err != nil {
		return nil, err
	}
	s.Saves = make([]Style, nSaves)
	for i := range s.Saves {
		if s.Saves[i], err = readStyle(r, hasField); err != nil {
			return nil, err
		}
	}
	if s.Flags&FlagGradCursor != 0 {
		tag, err := r.u8()
		if err != nil {
			return nil, err
		}
		if tag != 0x76 {
			return nil, fmt.Errorf("snapshot: unknown mid-op cursor tag 0x%x", tag)
		}
		c := &GradCursor{}
		for _, p := range []*int64{&c.I, &c.X, &c.Y, &c.W, &c.H} {
			if *p, err = r.i64(); err != nil {
				return nil, err
			}
		}
		if c.C1, err = r.u32(); err != nil {
			return nil, err
		}
		if c.C2, err = r.u32(); err != nil {
			return nil, err
		}
		if c.LineWidth, err = r.i64(); err != nil {
			return nil, err
		}
		s.GradCursor = c
	}
	if r.off != len(b) {
		return nil, fmt.Errorf("snapshot: trailing bytes (%d unread)", len(b)-r.off)
	}
	return s, nil
}

type writer struct{ b []byte }

func (w *writer) u8(v byte)    { w.b = append(w.b, v) }
func (w *writer) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *writer) i64(v int64)  { w.u64(uint64(v)) }

func writeStyle(w *writer, s *Style, withField bool) {
	for _, v := range s.M {
		w.i64(v)
	}
	w.u8(b2u(s.HasFill))
	w.u32(s.Fill)
	w.u8(b2u(s.HasStroke))
	w.u32(s.Stroke)
	w.i64(s.StrokeW)
	w.u8(s.ClipMode)
	if s.ClipMode != 0 {
		for _, v := range s.Clip {
			w.i64(v)
		}
	}
	w.u8(b2u(s.EvenOdd))
	w.u8(s.Brush)
	w.u8(s.Blend)
	w.u8(s.Cap)
	w.u8(s.Join)
	if s.Grad == nil {
		w.u8(0)
	} else {
		w.u8(1)
		w.u8(s.Grad.Kind)
		w.i64(s.Grad.X0)
		w.i64(s.Grad.Y0)
		w.i64(s.Grad.X1)
		w.i64(s.Grad.Y1)
		w.u8(byte(len(s.Grad.Stops)))
		for _, st := range s.Grad.Stops {
			w.u16(st.Off)
			w.u32(st.Color)
		}
	}
	// FFILL field-fill sub-record (KSN5/KSN6 only).
	if withField {
		if s.Field == nil {
			w.u8(0)
		} else {
			w.u8(1)
			w.u8(s.Field.Kind)
			w.i64(s.Field.Scale)
			w.i64(s.Field.Warp)
			w.u32(s.Field.ColorA)
			w.u32(s.Field.ColorB)
		}
	}
}

func b2u(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// EncodeSnapshot re-serializes a Snapshot; EncodeSnapshot(DecodeSnapshot(b))
// == b for any valid wire.
// Noise1 != nil selects the KSN4 magic + its post-flags section; a
// kind-0/none snapshot always encodes as bit-exact KSN3 (the back-compat law).
func EncodeSnapshot(s *Snapshot) []byte {
	w := &writer{b: make([]byte, 0, 4096)}
	// Field fill is style state, so it can hide in ANY frame (current or a save).
	field := s.Style.Field != nil
	for i := range s.Saves {
		if s.Saves[i].Field != nil {
			field = true
		}
	}
	// magic ladder: KSN3 base · KSN4 +kind1 · KSN5 +field · KSN6 +field+kind1.
	switch {
	case field && s.Noise1 != nil:
		w.u32(MagicKSN6)
	case field:
		w.u32(MagicKSN5)
	case s.Noise1 != nil:
		w.u32(MagicKSN4)
	default:
		w.u32(MagicKSN3)
	}
	w.u8(s.Flags)
	if s.Noise1 != nil {
		w.u8(s.Noise1.Octaves)
		w.u32(s.Noise1.Gain)
	}
	w.u32(s.PC)
	w.u64(s.Steps)
	w.u32(s.TraitCount)
	w.u16(uint16(len(s.Stack)))
	for _, v := range s.Stack {
		w.i64(v)
	}
	w.u16(uint16(len(s.Locals)))
	for _, v := range s.Locals {
		w.i64(v)
	}
	w.u16(uint16(len(s.CallStack)))
	for _, v := range s.CallStack {
		w.u32(v)
	}
	w.u16(s.MemBound)
	w.u8(b2u(s.MemcapSet))
	w.u32(s.Seed)
	if s.Flags&FlagRv1 != 0 {
		for _, v := range s.Rv1Seed {
			w.u32(v)
		}
		w.u8(s.Rv1Inext)
		w.u8(s.Rv1Inextp)
	}
	w.u8(byte(len(s.Beziers)))
	for i := range s.Beziers {
		bz := &s.Beziers[i]
		w.u8(bz.Slot)
		for _, v := range []int64{bz.Ax, bz.Ay, bz.Bx, bz.By, bz.Cx, bz.Cy, bz.Dx, bz.Dy} {
			w.i64(v)
		}
		w.u16(uint16(len(bz.Arc)))
		for _, v := range bz.Arc {
			w.i64(v)
		}
	}
	w.u32(s.PathSegs)
	if s.Flags&FlagHasCurrent != 0 {
		w.i64(s.CurX)
		w.i64(s.CurY)
	}
	if s.Flags&FlagFlipH != 0 {
		w.i64(s.FlipH)
	}
	w.i64(s.GlyphEm)
	w.i64(s.GlyphOx)
	w.i64(s.GlyphOy)
	writeStyle(w, &s.Style, field)
	w.u8(byte(len(s.Saves)))
	for i := range s.Saves {
		writeStyle(w, &s.Saves[i], field)
	}
	if s.Flags&FlagGradCursor != 0 && s.GradCursor != nil {
		c := s.GradCursor
		w.u8(0x76)
		w.i64(c.I)
		w.i64(c.X)
		w.i64(c.Y)
		w.i64(c.W)
		w.i64(c.H)
		w.u32(c.C1)
		w.u32(c.C2)
		w.i64(c.LineWidth)
	}
	return w.b
}
