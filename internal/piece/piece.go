// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package piece reads the IKohiPiece interface from a contract: the program,
// the params, a token's seed and the render path that turns them into pixels.
package piece

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ownable-xyz/ownable-cli/internal/abi"
	"github.com/ownable-xyz/ownable-cli/internal/rpc"
)

// Family names, indexed by KohiRenderPath.family.
var familyNames = map[uint8]string{
	0: "GraphicsV1 (integer rasterizer)",
	1: "AGG (hard strokes)",
	2: "AGG (blended strokes)",
	3: "stamp (SDF brush)",
	4: "mesh",
	5: "traced mesh",
}

// FamilyName describes a rasterizer family.
func FamilyName(f uint8) string {
	if n, ok := familyNames[f]; ok {
		return n
	}
	return fmt.Sprintf("unknown family %d", f)
}

// NoiseKindName describes a noise kind.
func NoiseKindName(k uint8) string {
	switch k {
	case 0:
		return "NoiseV1"
	case 1:
		return "GradNoise"
	}
	return fmt.Sprintf("unknown noise kind %d", k)
}

// ErrNotKohiPiece means the contract does not declare IKohiPiece via ERC-165.
var ErrNotKohiPiece = errors.New("contract does not declare IKohiPiece (ERC-165 0x9270a11e)")

// Piece is a contract that implements IKohiPiece.
type Piece struct {
	Address string
	c       *rpc.Client
	block   string
}

// Open checks that addr has code and declares IKohiPiece through ERC-165.
func Open(ctx context.Context, c *rpc.Client, addr string) (*Piece, error) {
	a, err := abi.ParseAddress(addr)
	if err != nil {
		return nil, err
	}
	p := &Piece{Address: a.String(), c: c, block: "latest"}
	code, err := c.Code(ctx, p.Address, p.block)
	if err != nil {
		return nil, err
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("no contract at %s on this chain", p.Address)
	}
	ok, err := p.Supports(ctx, abi.IKohiPieceID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotKohiPiece
	}
	return p, nil
}

func (p *Piece) call(ctx context.Context, data []byte) ([]byte, error) {
	// Getters are cheap; a generous limit still keeps a broken contract bounded.
	return p.c.EthCall(ctx, p.Address, data, 30_000_000, p.block)
}

// Supports is ERC-165 supportsInterface; a revert or empty answer is false.
func (p *Piece) Supports(ctx context.Context, id [4]byte) (bool, error) {
	ret, err := p.call(ctx, abi.EncodeSupportsInterface(id))
	if err != nil {
		var rev *rpc.RevertError
		if errors.As(err, &rev) {
			return false, nil
		}
		return false, err
	}
	if len(ret) < 32 {
		return false, nil
	}
	v, err := abi.NewRet(ret).BoolAt(0)
	if err != nil {
		return false, nil
	}
	return v, nil
}

// Program reads kohiProgram().
func (p *Piece) Program(ctx context.Context) ([]byte, error) {
	ret, err := p.call(ctx, abi.EncodeNoArgs("kohiProgram"))
	if err != nil {
		return nil, fmt.Errorf("kohiProgram: %w", err)
	}
	return abi.DecodeBytes(ret)
}

// Params reads kohiParams().
func (p *Piece) Params(ctx context.Context) ([]int64, error) {
	ret, err := p.call(ctx, abi.EncodeNoArgs("kohiParams"))
	if err != nil {
		return nil, fmt.Errorf("kohiParams: %w", err)
	}
	return abi.NewRet(ret).I64ArrayAt(0)
}

// Seed reads kohiSeed(tokenId).
func (p *Piece) Seed(ctx context.Context, token *big.Int) (int32, error) {
	ret, err := p.call(ctx, abi.EncodeTokenCall("kohiSeed", token))
	if err != nil {
		return 0, fmt.Errorf("kohiSeed(%s): %w (does the token exist?)", token, err)
	}
	return abi.NewRet(ret).I32At(0)
}

// RenderPath reads kohiRenderPath().
func (p *Piece) RenderPath(ctx context.Context) (*abi.RenderPath, error) {
	ret, err := p.call(ctx, abi.EncodeNoArgs("kohiRenderPath"))
	if err != nil {
		return nil, fmt.Errorf("kohiRenderPath: %w", err)
	}
	return abi.DecodeRenderPath(ret)
}

// TokenString reads one of the (uint256) string getters: kohiName or kohiAttributes.
func (p *Piece) TokenString(ctx context.Context, fn string, token *big.Int) (string, error) {
	ret, err := p.call(ctx, abi.EncodeTokenCall(fn, token))
	if err != nil {
		return "", fmt.Errorf("%s(%s): %w", fn, token, err)
	}
	return abi.DecodeString(ret)
}

// String reads a zero-argument string getter (name, symbol, kohiDescription).
// ok is false when the contract does not answer.
func (p *Piece) String(ctx context.Context, fn string) (string, bool) {
	ret, err := p.call(ctx, abi.EncodeNoArgs(fn))
	if err != nil {
		return "", false
	}
	s, err := abi.DecodeString(ret)
	return s, err == nil
}

// TotalSupply reads totalSupply(); ok is false when the contract has none.
func (p *Piece) TotalSupply(ctx context.Context) (*big.Int, bool) {
	ret, err := p.call(ctx, abi.EncodeNoArgs("totalSupply"))
	if err != nil {
		return nil, false
	}
	v, err := abi.NewRet(ret).BigAt(0)
	return v, err == nil
}

// TokenURI reads the ERC-721 tokenURI(tokenId) under the given gas limit.
// Metadata that embeds a player page can take far more gas than the other
// getters, so the caller picks the limit.
func (p *Piece) TokenURI(ctx context.Context, token *big.Int, gas uint64) (string, error) {
	ret, err := p.c.EthCall(ctx, p.Address, abi.EncodeTokenCall("tokenURI", token), gas, p.block)
	if err != nil {
		return "", fmt.Errorf("tokenURI(%s): %w", token, err)
	}
	return abi.DecodeString(ret)
}
