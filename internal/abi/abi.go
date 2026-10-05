// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package abi is a small Ethereum ABI codec for the fixed set of view
// functions this tool calls. It is not a general ABI engine: each call gets an
// explicit encoder and decoder over 32-byte words, so the wire layout can be
// read and checked against the Solidity signatures directly.
package abi

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Selector is the first 4 bytes of keccak256(signature).
func Selector(sig string) [4]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(sig))
	var out [4]byte
	copy(out[:], h.Sum(nil)[:4])
	return out
}

// Address is a 20-byte account address.
type Address [20]byte

// ParseAddress accepts a 0x-prefixed 40-hex-character address.
func ParseAddress(s string) (Address, error) {
	var a Address
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return a, fmt.Errorf("address %q: want 0x followed by 40 hex characters", s)
	}
	s = s[2:]
	if len(s) != 40 {
		return a, fmt.Errorf("address: want 40 hex characters, got %d", len(s))
	}
	for i := 0; i < 20; i++ {
		hi, ok1 := hexNib(s[i*2])
		lo, ok2 := hexNib(s[i*2+1])
		if !ok1 || !ok2 {
			return a, fmt.Errorf("address: bad hex at %d", i*2)
		}
		a[i] = hi<<4 | lo
	}
	return a, nil
}

func hexNib(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// String renders the address in EIP-55 mixed-case checksum form.
func (a Address) String() string {
	const hexd = "0123456789abcdef"
	lower := make([]byte, 40)
	for i, b := range a {
		lower[i*2], lower[i*2+1] = hexd[b>>4], hexd[b&0xf]
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(lower)
	sum := h.Sum(nil)
	out := []byte("0x")
	for i, c := range lower {
		nib := sum[i/2]
		if i%2 == 0 {
			nib >>= 4
		}
		if c >= 'a' && nib&0xf >= 8 {
			c -= 32
		}
		out = append(out, c)
	}
	return string(out)
}

// IsZero reports whether the address is address(0).
func (a Address) IsZero() bool { return a == Address{} }

// ---- encoding --------------------------------------------------------------

// enc builds calldata as a head of static words and offset slots plus a tail
// of dynamic data.
type enc struct {
	head []byte
	tail []byte
	dyn  []int // head offsets to patch with tail positions
	at   []int // tail position for each dyn entry
}

func word(v uint64) []byte {
	var w [32]byte
	binary.BigEndian.PutUint64(w[24:], v)
	return w[:]
}

func wordBig(v *big.Int) []byte {
	var w [32]byte
	v.FillBytes(w[:])
	return w[:]
}

// wordI64 sign-extends an int64 to a 32-byte two's-complement word.
func wordI64(v int64) []byte {
	var w [32]byte
	if v < 0 {
		for i := range w[:24] {
			w[i] = 0xff
		}
	}
	binary.BigEndian.PutUint64(w[24:], uint64(v))
	return w[:]
}

func padded(b []byte) []byte {
	out := make([]byte, (len(b)+31)/32*32)
	copy(out, b)
	return out
}

func (e *enc) static(w []byte) { e.head = append(e.head, w...) }

func (e *enc) dynamic(data []byte) {
	e.dyn = append(e.dyn, len(e.head))
	e.at = append(e.at, len(e.tail))
	e.head = append(e.head, make([]byte, 32)...)
	e.tail = append(e.tail, data...)
}

func bytesData(b []byte) []byte {
	return append(word(uint64(len(b))), padded(b)...)
}

func i64ArrayData(vs []int64) []byte {
	out := word(uint64(len(vs)))
	for _, v := range vs {
		out = append(out, wordI64(v)...)
	}
	return out
}

// bytesArrayData is the tail encoding of bytes[] (element offsets are relative
// to the start of the element head, after the length word).
func bytesArrayData(items [][]byte) []byte {
	n := len(items)
	head := make([]byte, 0, 32*n)
	var tail []byte
	for _, it := range items {
		head = append(head, word(uint64(32*n+len(tail)))...)
		tail = append(tail, bytesData(it)...)
	}
	return append(word(uint64(n)), append(head, tail...)...)
}

func (e *enc) finish(sel [4]byte) []byte {
	headLen := len(e.head)
	for i, off := range e.dyn {
		binary.BigEndian.PutUint64(e.head[off+24:off+32], uint64(headLen+e.at[i]))
	}
	out := make([]byte, 0, 4+headLen+len(e.tail))
	out = append(out, sel[:]...)
	out = append(out, e.head...)
	return append(out, e.tail...)
}

// ---- decoding --------------------------------------------------------------

// Ret is a cursor over ABI return data.
type Ret struct{ b []byte }

// NewRet wraps return data.
func NewRet(b []byte) *Ret { return &Ret{b: b} }

func (r *Ret) wordAt(off int) ([]byte, error) {
	if off < 0 || off+32 > len(r.b) {
		return nil, fmt.Errorf("abi: return data truncated at byte %d (len %d)", off, len(r.b))
	}
	return r.b[off : off+32], nil
}

func u64Word(w []byte) (uint64, error) {
	for _, b := range w[:24] {
		if b != 0 {
			return 0, fmt.Errorf("abi: word exceeds 64 bits")
		}
	}
	return binary.BigEndian.Uint64(w[24:]), nil
}

// U64At reads an unsigned word at slot i, rejecting values of 2^64 or more.
func (r *Ret) U64At(slot int) (uint64, error) {
	w, err := r.wordAt(slot * 32)
	if err != nil {
		return 0, err
	}
	return u64Word(w)
}

// BigAt reads a full 256-bit unsigned word at slot i.
func (r *Ret) BigAt(slot int) (*big.Int, error) {
	w, err := r.wordAt(slot * 32)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(w), nil
}

// BoolAt reads a bool word at slot i.
func (r *Ret) BoolAt(slot int) (bool, error) {
	v, err := r.U64At(slot)
	if err != nil {
		return false, err
	}
	if v > 1 {
		return false, fmt.Errorf("abi: bool word is %d", v)
	}
	return v == 1, nil
}

// AddressAt reads an address word at slot i.
func (r *Ret) AddressAt(slot int) (Address, error) {
	var a Address
	w, err := r.wordAt(slot * 32)
	if err != nil {
		return a, err
	}
	for _, b := range w[:12] {
		if b != 0 {
			return a, fmt.Errorf("abi: address word has high bits set")
		}
	}
	copy(a[:], w[12:])
	return a, nil
}

// I32At reads a signed int32 word at slot i, checking the sign extension.
func (r *Ret) I32At(slot int) (int32, error) {
	w, err := r.wordAt(slot * 32)
	if err != nil {
		return 0, err
	}
	v := int32(binary.BigEndian.Uint32(w[28:]))
	if string(wordI64(int64(v))) != string(w) {
		return 0, fmt.Errorf("abi: int32 word is not sign-extended")
	}
	return v, nil
}

// BytesAt follows the offset word at slot i to a bytes (or string) tail.
func (r *Ret) BytesAt(slot int) ([]byte, error) {
	off, err := r.U64At(slot)
	if err != nil {
		return nil, err
	}
	if off > uint64(len(r.b)) {
		return nil, fmt.Errorf("abi: bytes offset %d out of range", off)
	}
	lw, err := r.wordAt(int(off))
	if err != nil {
		return nil, err
	}
	n, err := u64Word(lw)
	if err != nil {
		return nil, err
	}
	start := int(off) + 32
	if n > uint64(len(r.b)-start) {
		return nil, fmt.Errorf("abi: bytes tail truncated (%d + %d > %d)", start, n, len(r.b))
	}
	out := make([]byte, n)
	copy(out, r.b[start:start+int(n)])
	return out, nil
}

// I64ArrayAt follows the offset word at slot i to an int64[] tail.
func (r *Ret) I64ArrayAt(slot int) ([]int64, error) {
	off, err := r.U64At(slot)
	if err != nil {
		return nil, err
	}
	if off > uint64(len(r.b)) {
		return nil, fmt.Errorf("abi: array offset %d out of range", off)
	}
	lw, err := r.wordAt(int(off))
	if err != nil {
		return nil, err
	}
	n, err := u64Word(lw)
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b))/32 {
		return nil, fmt.Errorf("abi: array length %d out of range", n)
	}
	out := make([]int64, n)
	for i := range out {
		w, err := r.wordAt(int(off) + 32 + i*32)
		if err != nil {
			return nil, err
		}
		v := int64(binary.BigEndian.Uint64(w[24:]))
		if string(wordI64(v)) != string(w) {
			return nil, fmt.Errorf("abi: int64 element %d is not sign-extended", i)
		}
		out[i] = v
	}
	return out, nil
}

// ---- the call set ------------------------------------------------------------

var (
	selSupportsInterface = Selector("supportsInterface(bytes4)")
	selKohiProgram       = Selector("kohiProgram()")
	selKohiParams        = Selector("kohiParams()")
	selKohiSeed          = Selector("kohiSeed(uint256)")
	selKohiRenderPath    = Selector("kohiRenderPath()")
	selKohiName          = Selector("kohiName(uint256)")
	selKohiDescription   = Selector("kohiDescription()")
	selKohiAttributes    = Selector("kohiAttributes(uint256)")
	selName              = Selector("name()")
	selSymbol            = Selector("symbol()")
	selTotalSupply       = Selector("totalSupply()")
	selOwnerOf           = Selector("ownerOf(uint256)")
	selTokenURI          = Selector("tokenURI(uint256)")
	selNoiseLib          = Selector("noiseLib()")
	selExecute           = Selector("execute(bytes,int32,int64[])")
	selExecuteChunk      = Selector("executeChunk(bytes,int32,int64[],bytes,bytes[],uint256)")
	selBuildTableRange   = Selector("buildTableRange(uint256,uint256)")
	selRenderTileP       = Selector("renderTileP(bytes,int256,int256,int256,int256,uint256,uint256,bytes)")
)

// IKohiPieceID is the ERC-165 interface id of IKohiPiece: the XOR of the
// selectors of its seven functions.
var IKohiPieceID = func() [4]byte {
	var id [4]byte
	for _, s := range [][4]byte{selKohiProgram, selKohiParams, selKohiSeed, selKohiRenderPath,
		selKohiName, selKohiDescription, selKohiAttributes} {
		for i := range id {
			id[i] ^= s[i]
		}
	}
	return id
}()

// ERC165ID is the interface id of ERC-165 itself.
var ERC165ID = selSupportsInterface

// EncodeSupportsInterface is ERC-165 supportsInterface(id).
func EncodeSupportsInterface(id [4]byte) []byte {
	e := &enc{}
	var w [32]byte
	copy(w[:4], id[:])
	e.static(w[:])
	return e.finish(selSupportsInterface)
}

var noArgs = map[string][4]byte{
	"kohiProgram": selKohiProgram, "kohiParams": selKohiParams, "kohiRenderPath": selKohiRenderPath,
	"kohiDescription": selKohiDescription, "name": selName, "symbol": selSymbol,
	"totalSupply": selTotalSupply, "noiseLib": selNoiseLib,
}

// EncodeNoArgs returns calldata for one of the zero-argument getters.
func EncodeNoArgs(fn string) []byte {
	sel, ok := noArgs[fn]
	if !ok {
		panic("abi: unknown getter " + fn)
	}
	return sel[:]
}

var tokenArgs = map[string][4]byte{
	"kohiSeed": selKohiSeed, "kohiName": selKohiName, "kohiAttributes": selKohiAttributes,
	"ownerOf": selOwnerOf, "tokenURI": selTokenURI,
}

// EncodeTokenCall returns calldata for one of the (uint256 tokenId) getters.
func EncodeTokenCall(fn string, tokenID *big.Int) []byte {
	sel, ok := tokenArgs[fn]
	if !ok {
		panic("abi: unknown token getter " + fn)
	}
	e := &enc{}
	e.static(wordBig(tokenID))
	return e.finish(sel)
}

// RenderPath mirrors the KohiRenderPath struct.
type RenderPath struct {
	Family     uint8
	NoiseKind  uint8
	Runner     Address
	StepRunner Address
	Renderer   Address
}

// DecodeRenderPath decodes kohiRenderPath(): a static tuple, so five words.
func DecodeRenderPath(ret []byte) (*RenderPath, error) {
	r := NewRet(ret)
	fam, err := r.U64At(0)
	if err != nil {
		return nil, err
	}
	nk, err := r.U64At(1)
	if err != nil {
		return nil, err
	}
	if fam > 255 || nk > 255 {
		return nil, fmt.Errorf("abi: render path family/noiseKind out of uint8 range")
	}
	p := &RenderPath{Family: uint8(fam), NoiseKind: uint8(nk)}
	if p.Runner, err = r.AddressAt(2); err != nil {
		return nil, err
	}
	if p.StepRunner, err = r.AddressAt(3); err != nil {
		return nil, err
	}
	if p.Renderer, err = r.AddressAt(4); err != nil {
		return nil, err
	}
	return p, nil
}

// EncodeExecute is KVMRunner.execute(program, seed, params): the one-shot
// interpreter. Big programs need more gas than most nodes allow.
func EncodeExecute(program []byte, seed int32, params []int64) []byte {
	e := &enc{}
	e.dynamic(bytesData(program))
	e.static(wordI64(int64(seed)))
	e.dynamic(i64ArrayData(params))
	return e.finish(selExecute)
}

// EncodeExecuteChunk is KVMStepRunner.executeChunk(program, seed, params,
// snapshot, blobs, stepBudget), blobs = [memImage, noiseTable, pathBlob].
func EncodeExecuteChunk(program []byte, seed int32, params []int64, snapshot []byte, blobs [][]byte, stepBudget uint64) []byte {
	e := &enc{}
	e.dynamic(bytesData(program))
	e.static(wordI64(int64(seed)))
	e.dynamic(i64ArrayData(params))
	e.dynamic(bytesData(snapshot))
	e.dynamic(bytesArrayData(blobs))
	e.static(word(stepBudget))
	return e.finish(selExecuteChunk)
}

// ExecuteChunkResult mirrors the executeChunk return values.
type ExecuteChunkResult struct {
	ChunkStream  []byte
	NextSnapshot []byte
	MemDelta     []byte
	PathDelta    []byte
	Done         bool
}

// DecodeExecuteChunk decodes executeChunk's five return values.
func DecodeExecuteChunk(ret []byte) (*ExecuteChunkResult, error) {
	r := NewRet(ret)
	var out ExecuteChunkResult
	var err error
	if out.ChunkStream, err = r.BytesAt(0); err != nil {
		return nil, err
	}
	if out.NextSnapshot, err = r.BytesAt(1); err != nil {
		return nil, err
	}
	if out.MemDelta, err = r.BytesAt(2); err != nil {
		return nil, err
	}
	if out.PathDelta, err = r.BytesAt(3); err != nil {
		return nil, err
	}
	if out.Done, err = r.BoolAt(4); err != nil {
		return nil, err
	}
	return &out, nil
}

// EncodeBuildTableRange is NoiseV1Ext.buildTableRange(g, count): one slice of
// the 4096-entry noise table, resuming from the generator state g.
func EncodeBuildTableRange(g *big.Int, count uint64) []byte {
	e := &enc{}
	e.static(wordBig(g))
	e.static(word(count))
	return e.finish(selBuildTableRange)
}

// DecodeBuildTableRange returns the table entries and the next generator state.
func DecodeBuildTableRange(ret []byte) ([]int64, *big.Int, error) {
	r := NewRet(ret)
	entries, err := r.I64ArrayAt(0)
	if err != nil {
		return nil, nil, err
	}
	g, err := r.BigAt(1)
	if err != nil {
		return nil, nil, err
	}
	return entries, g, nil
}

// StampWindow is the window and op range of one StampRenderer.renderTileP
// call. The window is in y-up canvas coordinates.
type StampWindow struct {
	X0, Y0, W, H     int64
	OpStart, OpCount uint64
}

// EncodeRenderTileP is StampRenderer.renderTileP(stream, winX0, winY0, winW,
// winH, opStart, opCount, prior).
func EncodeRenderTileP(stream []byte, w StampWindow, prior []byte) []byte {
	e := &enc{}
	e.dynamic(bytesData(stream))
	e.static(wordI64(w.X0))
	e.static(wordI64(w.Y0))
	e.static(wordI64(w.W))
	e.static(wordI64(w.H))
	e.static(word(w.OpStart))
	e.static(word(w.OpCount))
	e.dynamic(bytesData(prior))
	return e.finish(selRenderTileP)
}

// DecodeRenderTileP returns the window BGRA buffer and the stream's op total.
func DecodeRenderTileP(ret []byte) ([]byte, uint64, error) {
	r := NewRet(ret)
	buf, err := r.BytesAt(0)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.U64At(1)
	if err != nil {
		return nil, 0, err
	}
	return buf, total, nil
}

// DecodeString reads a lone string return value.
func DecodeString(ret []byte) (string, error) {
	b, err := NewRet(ret).BytesAt(0)
	return string(b), err
}

// DecodeBytes reads a lone bytes return value.
func DecodeBytes(ret []byte) ([]byte, error) {
	return NewRet(ret).BytesAt(0)
}

// Encoder builds ABI-encoded values without a selector, as a contract would
// return them. The tests use it to play the part of a contract.
type Encoder struct{ e enc }

// Bytes appends a bytes value.
func (x *Encoder) Bytes(b []byte) *Encoder { x.e.dynamic(bytesData(b)); return x }

// Uint appends a uint value.
func (x *Encoder) Uint(v uint64) *Encoder { x.e.static(word(v)); return x }

// Int appends a signed value.
func (x *Encoder) Int(v int64) *Encoder { x.e.static(wordI64(v)); return x }

// Bool appends a bool value.
func (x *Encoder) Bool(v bool) *Encoder {
	if v {
		return x.Uint(1)
	}
	return x.Uint(0)
}

// Int64s appends an int64[] value.
func (x *Encoder) Int64s(vs []int64) *Encoder { x.e.dynamic(i64ArrayData(vs)); return x }

// Done returns the encoding.
func (x *Encoder) Done() []byte { return x.e.finish([4]byte{})[4:] }

// I64At reads a signed int64 word at slot i.
func (r *Ret) I64At(slot int) (int64, error) {
	w, err := r.wordAt(slot * 32)
	if err != nil {
		return 0, err
	}
	v := int64(binary.BigEndian.Uint64(w[24:]))
	if string(wordI64(v)) != string(w) {
		return 0, fmt.Errorf("abi: int64 word is not sign-extended")
	}
	return v, nil
}
