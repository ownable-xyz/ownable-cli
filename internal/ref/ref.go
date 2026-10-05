// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package ref finds the reference image a render is verified against: a local
// file, an https:// URL, an IPFS path fetched through public gateways, or the
// "image" of a token's ERC-721 metadata. Whatever it finds is the claim under
// test; the render computed by the chain is the trust root.
package ref

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Kind says where a reference lives.
type Kind int

const (
	// Local is a file on this machine.
	Local Kind = iota
	// HTTP is an http:// or https:// URL.
	HTTP
	// IPFS is a CID plus an optional path inside it, fetched through gateways.
	IPFS
	// Data is a data: URI; the bytes are in the URI itself.
	Data
)

// Ref is a parsed reference location.
type Ref struct {
	Kind Kind
	Path string // Local: the file path
	URL  string // HTTP: the URL
	CID  string // IPFS: the root CID
	Sub  string // IPFS: the path inside the CID, without a leading slash
	Raw  string // the text it was parsed from (Data: the whole URI)
}

// String is the canonical name of the reference: the cache key and what the
// report prints.
func (r Ref) String() string {
	switch r.Kind {
	case Local:
		return r.Path
	case HTTP:
		return r.URL
	case IPFS:
		if r.Sub == "" {
			return "ipfs://" + r.CID
		}
		return "ipfs://" + r.CID + "/" + r.Sub
	case Data:
		if len(r.Raw) > 48 {
			return r.Raw[:48] + "..."
		}
		return r.Raw
	}
	return r.Raw
}

// Parse reads a reference given on the command line or found in metadata:
//
//	./file.png, C:\file.png         a local file (a path that exists wins)
//	https://host/path               an HTTP(S) URL
//	ipfs://CID/path, ipfs://ipfs/CID/path, /ipfs/CID/path
//	CID, CID/path                   a bare IPFS CID (v0 Qm... or v1 base32 b...)
//	data:...                        a data: URI
func Parse(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty reference")
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "data:"):
		return Ref{Kind: Data, Raw: s}, nil
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return Ref{}, fmt.Errorf("bad URL %q", s)
		}
		return Ref{Kind: HTTP, URL: s, Raw: s}, nil
	case strings.HasPrefix(lower, "ipfs://"):
		rest := s[len("ipfs://"):]
		if strings.HasPrefix(rest, "ipfs/") { // the old ipfs://ipfs/CID form
			rest = rest[len("ipfs/"):]
		}
		return ipfsRef(rest, s)
	case strings.HasPrefix(s, "/ipfs/"):
		return ipfsRef(s[len("/ipfs/"):], s)
	}
	if _, err := os.Stat(s); err == nil {
		return Ref{Kind: Local, Path: s, Raw: s}, nil
	}
	first, _, _ := strings.Cut(s, "/")
	if IsCID(first) {
		return ipfsRef(s, s)
	}
	return Ref{Kind: Local, Path: s, Raw: s}, nil
}

func ipfsRef(rest, raw string) (Ref, error) {
	rest = strings.TrimLeft(rest, "/")
	// A query or fragment is not part of an IPFS path.
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	cid, sub, _ := strings.Cut(rest, "/")
	if !IsCID(cid) {
		return Ref{}, fmt.Errorf("%q: %q is not an IPFS CID", raw, cid)
	}
	return Ref{Kind: IPFS, CID: cid, Sub: strings.Trim(sub, "/"), Raw: raw}, nil
}

const (
	base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	base32Alphabet = "abcdefghijklmnopqrstuvwxyz234567"
)

// IsCID reports whether s has the shape of an IPFS CID: a CIDv0 (46 base58
// characters starting "Qm") or a CIDv1 in base32 ("b" and 50 or more
// lowercase base32 characters) or base36 ("k" and 50 or more of [0-9a-z]).
// It checks the shape only, which is enough to tell a CID from a file name.
func IsCID(s string) bool {
	switch {
	case len(s) == 46 && strings.HasPrefix(s, "Qm"):
		return only(s, base58Alphabet)
	case len(s) >= 51 && s[0] == 'b':
		return only(s[1:], base32Alphabet)
	case len(s) >= 51 && s[0] == 'k':
		return only(s[1:], "0123456789abcdefghijklmnopqrstuvwxyz")
	}
	return false
}

func only(s, alphabet string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(alphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
