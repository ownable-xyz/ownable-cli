// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ref

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DataURI is a decoded data: URI.
type DataURI struct {
	MediaType string   // e.g. "application/json" (lower case, "" if absent)
	Params    []string // the parameters after the media type, e.g. ["utf8"]
	Base64    bool
	Body      []byte // the payload: base64-decoded, or the raw text after the comma
}

// ParseDataURI splits a data: URI. A base64 payload is decoded; any other
// payload is returned exactly as written, because the text forms in use do
// not agree on escaping (see MetadataJSON).
func ParseDataURI(s string) (*DataURI, error) {
	if len(s) < 5 || !strings.EqualFold(s[:5], "data:") {
		return nil, errors.New("not a data: URI")
	}
	head, body, ok := strings.Cut(s[5:], ",")
	if !ok {
		return nil, errors.New("data: URI without a comma")
	}
	d := &DataURI{}
	parts := strings.Split(head, ";")
	d.MediaType = strings.ToLower(strings.TrimSpace(parts[0]))
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if strings.EqualFold(p, "base64") {
			d.Base64 = true
			continue
		}
		d.Params = append(d.Params, p)
	}
	if d.Base64 {
		b, err := decodeBase64(body)
		if err != nil {
			return nil, fmt.Errorf("data: URI base64 payload: %w", err)
		}
		d.Body = b
		return d, nil
	}
	d.Body = []byte(body)
	return d, nil
}

// decodeBase64 accepts standard or URL-safe base64, padded or not, and
// ignores whitespace and percent-escaped padding.
func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, "%3D", "=")
	s = strings.ReplaceAll(s, "%3d", "=")
	var firstErr error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(s)
		if err == nil {
			return b, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// UnescapePlayer undoes the two escapes an on-chain player applies when it
// puts JSON text in a data: URI: "%25" back to "%" and "%23" back to "#",
// in one pass from left to right, so "%2523" becomes "%23" and nothing else
// changes. Everything else in the text (raw spaces, quotes, other "%xx"
// sequences that were in the JSON itself) is left exactly as it is.
func UnescapePlayer(s string) string {
	if !strings.Contains(s, "%2") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && s[i+1] == '2' {
			switch s[i+2] {
			case '5':
				b.WriteByte('%')
				i += 2
				continue
			case '3':
				b.WriteByte('#')
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// MetadataJSON reads a tokenURI. For a data: URI it returns the JSON text it
// carries and a nil Ref. For any other URI (https://, ipfs://, a bare CID) it
// returns the Ref where the JSON must be fetched from.
//
// The data: forms handled:
//
//	data:application/json;base64,<base64>   decoded as base64
//	data:application/json;utf8,<json>       the JSON as text, with only "%"
//	                                        and "#" escaped (UnescapePlayer)
//	data:application/json,<percent-encoded> fully percent-encoded JSON, used
//	                                        only when the text is not JSON
//	                                        after UnescapePlayer
func MetadataJSON(tokenURI string) ([]byte, *Ref, error) {
	tokenURI = strings.TrimSpace(tokenURI)
	if tokenURI == "" {
		return nil, nil, errors.New("tokenURI is empty")
	}
	if len(tokenURI) >= 5 && strings.EqualFold(tokenURI[:5], "data:") {
		d, err := ParseDataURI(tokenURI)
		if err != nil {
			return nil, nil, fmt.Errorf("tokenURI: %w", err)
		}
		if d.MediaType != "" && d.MediaType != "application/json" && d.MediaType != "text/plain" {
			return nil, nil, fmt.Errorf("tokenURI is a data: URI of type %q, not JSON metadata", d.MediaType)
		}
		if d.Base64 {
			return d.Body, nil, nil
		}
		text := UnescapePlayer(string(d.Body))
		if json.Valid([]byte(text)) {
			return []byte(text), nil, nil
		}
		// Not the player form: try a general percent-decoding of the original.
		if dec, err := url.PathUnescape(string(d.Body)); err == nil && json.Valid([]byte(dec)) {
			return []byte(dec), nil, nil
		}
		return nil, nil, errors.New("tokenURI: the data: URI does not hold valid JSON")
	}
	r, err := Parse(tokenURI)
	if err != nil {
		return nil, nil, fmt.Errorf("tokenURI: %w", err)
	}
	if r.Kind == Local {
		return nil, nil, fmt.Errorf("tokenURI %q is neither a data: URI nor a URL", short(tokenURI))
	}
	return nil, &r, nil
}

// ImageField returns the "image" of ERC-721 metadata JSON.
func ImageField(metadata []byte) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &m); err != nil {
		return "", fmt.Errorf("metadata is not a JSON object: %w", err)
	}
	raw, ok := m["image"]
	if !ok {
		if _, has := m["image_data"]; has {
			return "", errors.New(`metadata has "image_data" but no "image"; pass --against with a PNG`)
		}
		return "", errors.New(`metadata has no "image" field; pass --against with a PNG`)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf(`metadata "image" is not a string: %w`, err)
	}
	if strings.TrimSpace(s) == "" {
		return "", errors.New(`metadata "image" is empty`)
	}
	return strings.TrimSpace(s), nil
}

func short(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
