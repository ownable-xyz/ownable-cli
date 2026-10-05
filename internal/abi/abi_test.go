// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package abi

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"
)

func TestSelectors(t *testing.T) {
	cases := map[string]string{
		"totalSupply()":                "18160ddd",
		"supportsInterface(bytes4)":    "01ffc9a7",
		"execute(bytes,int32,int64[])": "8f98ce2c",
		"transfer(address,uint256)":    "a9059cbb",
	}
	for sig, want := range cases {
		got := Selector(sig)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("Selector(%q) = %x, want %s", sig, got, want)
		}
	}
}

// The IKohiPiece interface id is fixed by the interface; a change here means
// a selector string drifted from the Solidity signature.
func TestIKohiPieceID(t *testing.T) {
	if hex.EncodeToString(IKohiPieceID[:]) != "9270a11e" {
		t.Fatalf("IKohiPieceID = %x, want 9270a11e", IKohiPieceID)
	}
}

func TestAddressChecksum(t *testing.T) {
	// EIP-55 test vectors.
	for _, s := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	} {
		a, err := ParseAddress(s)
		if err != nil {
			t.Fatal(err)
		}
		if a.String() != s {
			t.Errorf("checksum %s, want %s", a.String(), s)
		}
	}
	if _, err := ParseAddress("e56522Ebaf8E19c7E2B9468Cbef8aF14773F5F18"); err == nil {
		t.Error("address without 0x accepted")
	}
	if _, err := ParseAddress("0x1234"); err == nil {
		t.Error("short address accepted")
	}
}

func w(hexWord string) []byte {
	b, _ := hex.DecodeString(hexWord)
	out := make([]byte, 32-len(b))
	return append(out, b...)
}

// executeChunk's layout, built by hand: head of 6 words (4 offsets, seed,
// budget), then the tails in argument order.
func TestEncodeExecuteChunkLayout(t *testing.T) {
	prog := []byte{1, 2, 3}
	got := EncodeExecuteChunk(prog, -2, []int64{7}, nil, [][]byte{nil, {0xaa}}, 5)
	sel := Selector("executeChunk(bytes,int32,int64[],bytes,bytes[],uint256)")
	var want []byte
	want = append(want, sel[:]...)
	want = append(want, w("c0")...) // program at 6*32
	want = append(want, bytes.Repeat([]byte{0xff}, 31)...)
	want = append(want, 0xfe)         // seed -2, sign-extended
	want = append(want, w("0100")...) // params at 0xc0 + 64
	want = append(want, w("0140")...) // snapshot at +64
	want = append(want, w("0160")...) // blobs at +32
	want = append(want, w("05")...)   // stepBudget
	// program
	want = append(want, w("03")...)
	want = append(want, append([]byte{1, 2, 3}, make([]byte, 29)...)...)
	// params
	want = append(want, w("01")...)
	want = append(want, w("07")...)
	// snapshot (empty)
	want = append(want, w("00")...)
	// blobs: length 2, two offsets relative to the element head, then elements
	want = append(want, w("02")...)
	want = append(want, w("40")...)
	want = append(want, w("60")...)
	want = append(want, w("00")...) // blob 0: empty
	want = append(want, w("01")...) // blob 1: one byte
	want = append(want, append([]byte{0xaa}, make([]byte, 31)...)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("calldata mismatch\n got %x\nwant %x", got, want)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	// (bytes, bytes, bytes, bytes, bool) as executeChunk returns it.
	e := &enc{}
	e.dynamic(bytesData([]byte("stream")))
	e.dynamic(bytesData([]byte("snap")))
	e.dynamic(bytesData(nil))
	e.dynamic(bytesData([]byte{1}))
	e.static(word(1))
	ret := e.finish([4]byte{})[4:]
	out, err := DecodeExecuteChunk(ret)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.ChunkStream) != "stream" || string(out.NextSnapshot) != "snap" ||
		len(out.MemDelta) != 0 || !bytes.Equal(out.PathDelta, []byte{1}) || !out.Done {
		t.Fatalf("bad decode %+v", out)
	}
	if _, err := DecodeExecuteChunk(ret[:len(ret)-40]); err == nil {
		t.Fatal("truncated return data accepted")
	}
}

func TestDecodeRenderPath(t *testing.T) {
	var ret []byte
	ret = append(ret, w("03")...)
	ret = append(ret, w("00")...)
	ret = append(ret, w("334a2e292da5499cf955280e4daef48852e330c6")...)
	ret = append(ret, w("1d8a97a40dfc234578cb6f4aa13a99826b67e228")...)
	ret = append(ret, w("454ead37e6254101f9abe416a4ff25caf75cbaa1")...)
	p, err := DecodeRenderPath(ret)
	if err != nil {
		t.Fatal(err)
	}
	if p.Family != 3 || p.NoiseKind != 0 || p.Renderer.String() != "0x454ead37E6254101f9abe416a4Ff25CAF75cbaa1" {
		t.Fatalf("bad render path %+v", p)
	}
}

func TestSignedWords(t *testing.T) {
	e := &enc{}
	e.dynamic(i64ArrayData([]int64{-1, 1 << 40, -(1 << 62)}))
	ret := e.finish([4]byte{})[4:]
	vs, err := NewRet(ret).I64ArrayAt(0)
	if err != nil {
		t.Fatal(err)
	}
	if vs[0] != -1 || vs[1] != 1<<40 || vs[2] != -(1<<62) {
		t.Fatalf("bad int64s %v", vs)
	}
	seed := wordI64(-2027370766)
	v, err := NewRet(seed).I32At(0)
	if err != nil || v != -2027370766 {
		t.Fatalf("I32At = %d, %v", v, err)
	}
	bad := w("80000000") // positive 2^31 does not fit int32
	if _, err := NewRet(bad).I32At(0); err == nil {
		t.Fatal("non-sign-extended int32 accepted")
	}
}

func TestEncodeBuildTableRange(t *testing.T) {
	got := EncodeBuildTableRange(big.NewInt(442873733), 1600)
	if len(got) != 4+64 {
		t.Fatalf("len %d", len(got))
	}
	if new(big.Int).SetBytes(got[4:36]).Int64() != 442873733 || new(big.Int).SetBytes(got[36:68]).Int64() != 1600 {
		t.Fatal("bad words")
	}
}
