// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

package fork

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	got := Args(Options{Upstream: "https://rpc.example", Block: 23_456_789, Port: 54321})
	want := []string{
		"--fork-url", "https://rpc.example",
		"--fork-block-number", "23456789",
		"--host", "127.0.0.1",
		"--port", "54321",
		"--disable-block-gas-limit",
		"--memory-limit", "17179869184",
		"--no-request-size-limit",
		"--no-mining",
		"--no-rate-limit",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("Args =\n%q\nwant\n%q", got, want)
	}
	got = Args(Options{Upstream: "u", Block: 1, Port: 2, Host: "0.0.0.0", MemoryLimit: 7})
	s := strings.Join(got, " ")
	for _, part := range []string{"--host 0.0.0.0", "--memory-limit 7"} {
		if !strings.Contains(s, part) {
			t.Errorf("%q lacks %q", s, part)
		}
	}
}

func TestFindAnvil(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // an empty PATH: no anvil
	if _, err := FindAnvil(""); !errors.Is(err, ErrNoAnvil) {
		t.Fatalf("want ErrNoAnvil, got %v", err)
	}
	if !strings.Contains(ErrNoAnvil.Error(), "Foundry") {
		t.Fatal("the message should name Foundry")
	}
	if _, err := FindAnvil(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("a missing --anvil path was accepted")
	}
	p := filepath.Join(t.TempDir(), "anvil")
	os.WriteFile(p, nil, 0o755)
	if got, err := FindAnvil(p); err != nil || got != p {
		t.Fatalf("FindAnvil(%q) = %q, %v", p, got, err)
	}
}

func TestFreePort(t *testing.T) {
	p, err := FreePort()
	if err != nil || p <= 0 {
		t.Fatalf("%d %v", p, err)
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatalf("port %d not reusable: %v", p, err)
	}
	l.Close()
}

func TestBlockNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x165ec15"}`))
	}))
	defer srv.Close()
	b, err := BlockNumber(context.Background(), srv.URL)
	if err != nil || b != 0x165ec15 {
		t.Fatalf("%d %v", b, err)
	}
	if _, err := BlockNumber(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Fatal("no node, no error")
	}
}
