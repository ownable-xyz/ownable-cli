// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package ref

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	cidV1 = "bafybeifhe3zwculpcnkm7pvd75axjonsqfuc3eirjiv2lzotjl4pxwud2m"
	cidV0 = "QmYwAPJzv5CZsnA625s3Xf2nemtYgPpHdWEz79ojWnPbdG"
)

func TestIsCID(t *testing.T) {
	for _, s := range []string{cidV1, cidV0, "k51qzi5uqu5dlvj2baxnqndepeb86cbk3ng7n3i46uzyxzyqj2xjonzllnv0v8"} {
		if !IsCID(s) {
			t.Errorf("IsCID(%q) = false", s)
		}
	}
	for _, s := range []string{"", "1.png", "rakel-1.png", "Qm123", "bafy", strings.ToUpper(cidV1),
		cidV0[:45] + "0", // 0 is not base58
		"b" + strings.Repeat("1", 58)} {
		if IsCID(s) {
			t.Errorf("IsCID(%q) = true", s)
		}
	}
}

func TestParse(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "x.png")
	os.WriteFile(local, []byte("x"), 0o644)
	cases := []struct {
		in        string
		kind      Kind
		canonical string
	}{
		{local, Local, local},
		{"missing.png", Local, "missing.png"},
		{"https://example.org/a/1.png", HTTP, "https://example.org/a/1.png"},
		{"http://127.0.0.1:8080/x", HTTP, "http://127.0.0.1:8080/x"},
		{"ipfs://" + cidV1 + "/1.png", IPFS, "ipfs://" + cidV1 + "/1.png"},
		{"ipfs://" + cidV1, IPFS, "ipfs://" + cidV1},
		{"ipfs://ipfs/" + cidV1 + "/1.png", IPFS, "ipfs://" + cidV1 + "/1.png"},
		{"/ipfs/" + cidV1 + "/a/b.png", IPFS, "ipfs://" + cidV1 + "/a/b.png"},
		{cidV1, IPFS, "ipfs://" + cidV1},
		{cidV1 + "/1.png", IPFS, "ipfs://" + cidV1 + "/1.png"},
		{cidV0 + "/7.png", IPFS, "ipfs://" + cidV0 + "/7.png"},
		{"ipfs://" + cidV1 + "/1.png?filename=x", IPFS, "ipfs://" + cidV1 + "/1.png"},
		{"data:image/png;base64,AAAA", Data, "data:image/png;base64,AAAA"},
	}
	for _, c := range cases {
		r, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if r.Kind != c.kind || r.String() != c.canonical {
			t.Errorf("Parse(%q) = kind %d %q, want kind %d %q", c.in, r.Kind, r.String(), c.kind, c.canonical)
		}
	}
	for _, bad := range []string{"", "ipfs://notacid/1.png", "https://"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestUnescapePlayer(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"plain text":             "plain text",
		"Rakel %231":             "Rakel #1",
		"100%25":                 "100%",
		"%2523":                  "%23", // one pass: %25 -> %, then 23 stays
		"%252525":                "%2525",
		"%2323":                  "#23",
		"a%20b %2F %22":          "a%20b %2F %22", // other escapes are not undone
		"%2":                     "%2",
		"%25%23%2":               "%#%2",
		`{"a":"x %2523 y %23z"}`: `{"a":"x %23 y #z"}`,
	}
	for in, want := range cases {
		if got := UnescapePlayer(in); got != want {
			t.Errorf("UnescapePlayer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMetadataJSON(t *testing.T) {
	image := "ipfs://" + cidV1 + "/1.png"
	// The JSON as the contract built it, before the data: URI escaping.
	meta := `{"name":"Rakel #1","description":"paint, 100% dragged; a literal %23 here","image":"` + image +
		`","animation_url":"data:text/html,%3C!doctype%20html%3E"}`

	t.Run("base64", func(t *testing.T) {
		uri := "data:application/json;base64," + base64.StdEncoding.EncodeToString([]byte(meta))
		got, r, err := MetadataJSON(uri)
		if err != nil || r != nil || string(got) != meta {
			t.Fatalf("got %q %v %v", got, r, err)
		}
		if img, err := ImageField(got); err != nil || img != image {
			t.Fatalf("image %q %v", img, err)
		}
	})

	t.Run("utf8 player form", func(t *testing.T) {
		// The player escapes only % and # (in that order, so a literal
		// "%23" becomes "%2523").
		esc := strings.ReplaceAll(meta, "%", "%25")
		esc = strings.ReplaceAll(esc, "#", "%23")
		if !strings.Contains(esc, "%2523") || !strings.Contains(esc, "Rakel %231") {
			t.Fatalf("test setup: %q", esc)
		}
		got, r, err := MetadataJSON("data:application/json;utf8," + esc)
		if err != nil || r != nil {
			t.Fatalf("%v %v", r, err)
		}
		if string(got) != meta {
			t.Fatalf("got %q\nwant %q", got, meta)
		}
		if img, err := ImageField(got); err != nil || img != image {
			t.Fatalf("image %q %v", img, err)
		}
	})

	t.Run("fully percent-encoded", func(t *testing.T) {
		enc := strings.NewReplacer("{", "%7B", "}", "%7D", `"`, "%22", " ", "%20", "%", "%25", "#", "%23").Replace(meta)
		got, _, err := MetadataJSON("data:application/json," + enc)
		if err != nil {
			t.Fatal(err)
		}
		if img, err := ImageField(got); err != nil || img != image {
			t.Fatalf("image %q %v", img, err)
		}
	})

	t.Run("remote", func(t *testing.T) {
		for _, uri := range []string{"https://example.org/meta/1", "ipfs://" + cidV1 + "/1.json", cidV1 + "/1"} {
			got, r, err := MetadataJSON(uri)
			if err != nil || got != nil || r == nil {
				t.Fatalf("%s: %q %v %v", uri, got, r, err)
			}
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, uri := range []string{"", "data:image/png;base64,AAAA", "data:application/json;utf8,{not json", "just words"} {
			if _, _, err := MetadataJSON(uri); err == nil {
				t.Errorf("MetadataJSON(%q) succeeded", uri)
			}
		}
		for _, m := range []string{`{}`, `{"image":""}`, `{"image":3}`, `[1]`, `{"image_data":"<svg/>"}`} {
			if _, err := ImageField([]byte(m)); err == nil {
				t.Errorf("ImageField(%s) succeeded", m)
			}
		}
	})
}

func TestFetchGatewayFallback(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	defer bad.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		if r.URL.Path != "/ipfs/"+cidV1+"/1.png" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("PNGBYTES"))
	}))
	defer good.Close()

	cache := t.TempDir()
	f := &Fetcher{
		Gateways: []string{bad.URL + "/ipfs/", slow.URL + "/ipfs", good.URL + "/ipfs/"},
		Timeout:  200 * time.Millisecond,
		CacheDir: cache,
	}
	r, _ := Parse("ipfs://" + cidV1 + "/1.png")
	res, err := f.Fetch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != "PNGBYTES" || res.Gateway != good.URL+"/ipfs/" || res.Cached || len(res.Failed) != 2 {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(res.Failed[0], "429") || !strings.Contains(res.Failed[1], "timed out") {
		t.Fatalf("failures %q", res.Failed)
	}
	if res.SHA256 != sum([]byte("PNGBYTES")) || res.Source != "ipfs://"+cidV1+"/1.png" {
		t.Fatalf("got %+v", res)
	}

	// The second fetch comes from the cache and touches no gateway.
	hits := badHits.Load() + goodHits.Load()
	res2, err := f.Fetch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Cached || string(res2.Data) != "PNGBYTES" || res2.URL != res.URL || res2.Gateway != res.Gateway {
		t.Fatalf("second fetch %+v", res2)
	}
	if badHits.Load()+goodHits.Load() != hits {
		t.Fatal("cached fetch reached a gateway")
	}

	// A bare CID path and the ipfs:// form share one cache entry.
	r3, _ := Parse(cidV1 + "/1.png")
	if CacheKey(r3) != CacheKey(r) {
		t.Fatal("bare CID and ipfs:// differ in cache key")
	}

	// Every gateway failing is an error naming each attempt.
	f2 := &Fetcher{Gateways: []string{bad.URL + "/ipfs/"}, Timeout: time.Second}
	if _, err := f2.Fetch(context.Background(), r); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want an error naming the 429, got %v", err)
	}
}

func TestFetchHTTPAndLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Write(make([]byte, 2048))
			return
		}
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	f := &Fetcher{Timeout: time.Second, MaxBytes: 1024}
	r, _ := Parse(srv.URL + "/x.png")
	res, err := f.Fetch(context.Background(), r)
	if err != nil || string(res.Data) != "hello" || res.URL != srv.URL+"/x.png" || res.Gateway != "" {
		t.Fatalf("%+v %v", res, err)
	}
	rb, _ := Parse(srv.URL + "/big")
	if _, err := f.Fetch(context.Background(), rb); err == nil {
		t.Fatal("oversized download accepted")
	}

	p := filepath.Join(t.TempDir(), "a.png")
	os.WriteFile(p, []byte("local"), 0o644)
	rl, _ := Parse(p)
	res, err = f.Fetch(context.Background(), rl)
	if err != nil || string(res.Data) != "local" || res.URL != "" {
		t.Fatalf("%+v %v", res, err)
	}

	rd, _ := Parse("data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("inline")))
	res, err = f.Fetch(context.Background(), rd)
	if err != nil || string(res.Data) != "inline" {
		t.Fatalf("%+v %v", res, err)
	}
}
