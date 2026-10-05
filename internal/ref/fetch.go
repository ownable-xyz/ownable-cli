// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultGateways are tried in order for IPFS references. Public gateways
// come and go; pass --ipfs-gateway (repeatable) to use others, such as a
// local node's http://127.0.0.1:8080/ipfs/.
var DefaultGateways = []string{
	"https://ipfs.filebase.io/ipfs/",
	"https://gateway.pinata.cloud/ipfs/",
	"https://ipfs.io/ipfs/",
	"https://dweb.link/ipfs/",
}

// Fetcher downloads references, with gateway fallback and an on-disk cache.
type Fetcher struct {
	Gateways []string      // IPFS gateways, each ending in "/ipfs/" (or any prefix the CID is appended to)
	Timeout  time.Duration // per attempt (default 60s)
	CacheDir string        // "" disables the cache
	MaxBytes int64         // largest download accepted (default 256 MiB)
	HTTP     *http.Client
	Agent    string // User-Agent
}

// Result is a fetched reference.
type Result struct {
	Source  string // the canonical reference (ipfs://CID/path, the URL, the file path)
	URL     string // the URL actually read ("" for local files and data: URIs)
	Gateway string // the gateway that answered, for IPFS
	Data    []byte
	SHA256  string
	Cached  bool     // read from the cache, not downloaded in this run
	Failed  []string // gateways or attempts that failed before this one, with why
}

// CacheKey is the cache file name for a reference: the sha256 of its
// canonical string.
func CacheKey(r Ref) string {
	h := sha256.Sum256([]byte(r.String()))
	return hex.EncodeToString(h[:])
}

// DefaultCacheDir is <user cache dir>/ownable/refs.
func DefaultCacheDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "ownable", "refs")
}

func (f *Fetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return 60 * time.Second
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return 256 << 20
}

func (f *Fetcher) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

// Fetch reads a reference. Local files are read directly and data: URIs are
// decoded; HTTP and IPFS references come from the cache when present, else
// are downloaded (IPFS through each gateway in turn until one answers) and
// cached.
func (f *Fetcher) Fetch(ctx context.Context, r Ref) (*Result, error) {
	res := &Result{Source: r.String()}
	switch r.Kind {
	case Local:
		b, err := os.ReadFile(r.Path)
		if err != nil {
			return nil, err
		}
		res.Data = b
		res.SHA256 = sum(b)
		return res, nil
	case Data:
		d, err := ParseDataURI(r.Raw)
		if err != nil {
			return nil, err
		}
		res.Data = d.Body
		res.SHA256 = sum(d.Body)
		return res, nil
	}

	cachePath := ""
	if f.CacheDir != "" {
		cachePath = filepath.Join(f.CacheDir, CacheKey(r))
		if b, err := os.ReadFile(cachePath); err == nil && len(b) > 0 {
			res.Data, res.SHA256, res.Cached = b, sum(b), true
			if u, err := os.ReadFile(cachePath + ".url"); err == nil {
				res.URL, res.Gateway, _ = strings.Cut(strings.TrimSpace(string(u)), " ")
			}
			return res, nil
		}
	}

	var urls, gateways []string
	switch r.Kind {
	case HTTP:
		urls, gateways = []string{r.URL}, []string{""}
	case IPFS:
		gws := f.Gateways
		if len(gws) == 0 {
			gws = DefaultGateways
		}
		path := r.CID
		if r.Sub != "" {
			path += "/" + r.Sub
		}
		for _, g := range gws {
			if !strings.HasSuffix(g, "/") {
				g += "/"
			}
			urls = append(urls, g+path)
			gateways = append(gateways, g)
		}
	default:
		return nil, fmt.Errorf("cannot fetch reference kind %d", r.Kind)
	}

	for i, u := range urls {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, err := f.get(ctx, u)
		if err != nil {
			res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		res.URL, res.Gateway, res.Data, res.SHA256 = u, gateways[i], b, sum(b)
		if cachePath != "" {
			f.store(cachePath, b, u+" "+gateways[i])
		}
		return res, nil
	}
	return nil, fmt.Errorf("could not download %s:\n  %s", r, strings.Join(res.Failed, "\n  "))
}

func (f *Fetcher) get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if f.Agent != "" {
		req.Header.Set("User-Agent", f.Agent)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %s", f.timeout())
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 160))
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(strings.ReplaceAll(string(msg), "\n", " ")))
	}
	limit := f.maxBytes()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("timed out after %s", f.timeout())
		}
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	if len(b) == 0 {
		return nil, errors.New("empty response")
	}
	return b, nil
}

// store writes a cache entry atomically; a failure only costs a re-download.
func (f *Fetcher) store(path string, b []byte, origin string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return
	}
	os.WriteFile(path+".url", []byte(origin+"\n"), 0o644)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
