// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package hexutil converts between bytes and the 0x-prefixed hex strings that
// JSON-RPC speaks.
package hexutil

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Decode parses hex with or without a 0x prefix, ignoring ASCII whitespace.
func Decode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if strings.ContainsAny(s, " \t\r\n") {
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			if r != ' ' && r != '\t' && r != '\r' && r != '\n' {
				b.WriteRune(r)
			}
		}
		s = b.String()
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("hex: odd length %d", len(s))
	}
	return hex.DecodeString(s)
}

// Encode renders 0x-prefixed lowercase hex ("0x" for empty input).
func Encode(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// ReadFile loads a file of hex text (optional 0x, any whitespace).
func ReadFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(string(raw))
}
