// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// The client side of the executeChunk state protocol: three blobs the client
// holds between calls (the linear-memory image, the noise table, the open-path
// segments) and the per-chunk delta folds that carry them forward.
package stepkvm

import (
	"encoding/binary"
	"fmt"
)

// ApplyMemDelta folds a chunk's memDelta into the client-held memory image
// (v2 protocol: u16 memBound | u32 count | count x (u16 idx | 8 B BE value);
// an empty image materializes as memBound x 8 zeros first). Returns the
// (possibly newly allocated) image.
func ApplyMemDelta(img []byte, delta []byte) ([]byte, error) {
	if len(delta) == 0 {
		return img, nil
	}
	if len(delta) < 6 {
		return nil, fmt.Errorf("memDelta header truncated (%d B)", len(delta))
	}
	mb := int(binary.BigEndian.Uint16(delta[0:2]))
	cnt := int(binary.BigEndian.Uint32(delta[2:6]))
	if len(img) == 0 {
		img = make([]byte, mb*8)
	}
	if len(img) != mb*8 {
		return nil, fmt.Errorf("memDelta geometry %d != %d", mb, len(img)/8)
	}
	o := 6
	for i := 0; i < cnt; i++ {
		if o+10 > len(delta) {
			return nil, fmt.Errorf("memDelta truncated at write %d", i)
		}
		idx := int(binary.BigEndian.Uint16(delta[o : o+2]))
		if idx >= mb {
			return nil, fmt.Errorf("memDelta write %d out of bound (%d >= %d)", i, idx, mb)
		}
		copy(img[idx*8:idx*8+8], delta[o+2:o+10])
		o += 10
	}
	if o != len(delta) {
		return nil, fmt.Errorf("memDelta not fully consumed (%d of %d)", o, len(delta))
	}
	return img, nil
}

// ApplyPathDelta folds a chunk's pathDelta into the client-held open-path
// blob (v3 protocol: u8 cleared | u32 tailSegCount | tail seg bytes; empty
// delta = unchanged; cleared discards the accumulated blob first).
func ApplyPathDelta(blob []byte, delta []byte) ([]byte, error) {
	if len(delta) == 0 {
		return blob, nil
	}
	if len(delta) < 5 {
		return nil, fmt.Errorf("pathDelta header truncated (%d B)", len(delta))
	}
	tail := delta[5:]
	if delta[0] != 0 {
		out := make([]byte, len(tail))
		copy(out, tail)
		return out, nil
	}
	out := make([]byte, 0, len(blob)+len(tail))
	out = append(out, blob...)
	return append(out, tail...), nil
}

// Seg byte lengths in the canonical path-blob encoding (recorder tags:
// 1 M x,y | 2 L x,y | 3 C c1,c2,p | 4 Z).
func segLen(tag byte) int {
	switch tag {
	case 1, 2:
		return 17
	case 3:
		return 49
	case 4:
		return 1
	default:
		return 0
	}
}

// WalkPathBlob validates the client-held path blob parses to exactly
// segCount segs and returns the current point (last M/L/C endpoint) if any.
// The caller checks that point against the snapshot's own current point.
func WalkPathBlob(blob []byte, segCount uint32) (curX, curY int64, hasCur bool, err error) {
	o := 0
	n := uint32(0)
	for o < len(blob) {
		sl := segLen(blob[o])
		if sl == 0 {
			return 0, 0, false, fmt.Errorf("path blob: bad seg tag 0x%x @%d", blob[o], o)
		}
		if o+sl > len(blob) {
			return 0, 0, false, fmt.Errorf("path blob: truncated seg @%d", o)
		}
		if blob[o] != 4 { // M/L/C carry an endpoint as their trailing 16 bytes
			curX = int64(binary.BigEndian.Uint64(blob[o+sl-16 : o+sl-8]))
			curY = int64(binary.BigEndian.Uint64(blob[o+sl-8 : o+sl]))
			hasCur = true
		}
		o += sl
		n++
	}
	if n != segCount {
		return 0, 0, false, fmt.Errorf("path blob: %d segs != snapshot segCount %d", n, segCount)
	}
	return curX, curY, hasCur, nil
}
