// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package fork runs a local anvil node (Foundry) forked from an upstream
// endpoint at a pinned block, so a render executes against the same contract
// code and state on this machine, without the upstream node's gas cap and
// without a network round trip per call.
package fork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures a forked node.
type Options struct {
	Anvil    string // the anvil executable
	Upstream string // the endpoint the fork reads state from
	Block    uint64 // the pinned fork block
	Host     string // listen address (default 127.0.0.1)
	Port     int    // listen port
	// MemoryLimit is the EVM memory limit per execution in bytes
	// (default DefaultMemoryLimit).
	MemoryLimit uint64
	Log         io.Writer // anvil's output ("" discards it)
}

const (
	// CallGas is the gas limit sent with every call to a fork: high enough
	// that no render call is ever cut short. The fork runs with
	// --disable-block-gas-limit, so the block gas limit does not bound it.
	CallGas = 1 << 48
	// DefaultMemoryLimit is the EVM memory limit per call: 16 GiB.
	DefaultMemoryLimit = 16 << 30
)

// ErrNoAnvil means anvil was not found.
var ErrNoAnvil = errors.New("--fork needs anvil, Foundry's local node, on PATH (install Foundry: https://getfoundry.sh), or pass --anvil PATH, or start a node yourself and pass --fork-url")

// FindAnvil returns the anvil executable: explicit if given, else from PATH.
func FindAnvil(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--anvil %s: %w", explicit, err)
		}
		return explicit, nil
	}
	p, err := exec.LookPath("anvil")
	if err != nil {
		return "", ErrNoAnvil
	}
	return p, nil
}

// Args is the anvil command line for o (without the executable).
func Args(o Options) []string {
	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	mem := o.MemoryLimit
	if mem == 0 {
		mem = DefaultMemoryLimit
	}
	return []string{
		"--fork-url", o.Upstream,
		"--fork-block-number", strconv.FormatUint(o.Block, 10),
		"--host", host,
		"--port", strconv.Itoa(o.Port),
		"--disable-block-gas-limit",
		"--memory-limit", strconv.FormatUint(mem, 10),
		"--no-request-size-limit",
		"--no-mining",
		"--no-rate-limit",
	}
}

// FreePort asks the OS for a free TCP port on the loopback interface.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Node is a running anvil process.
type Node struct {
	URL   string
	Block uint64
	Args  []string

	cmd    *exec.Cmd
	exited chan struct{}
	once   sync.Once
}

// Start launches anvil and waits until it answers JSON-RPC at the pinned
// block, or fails if it exits or does not answer within wait. The process is
// killed when ctx is cancelled; call Stop when done in any case.
func Start(ctx context.Context, o Options, wait time.Duration) (*Node, error) {
	if o.Port == 0 {
		p, err := FreePort()
		if err != nil {
			return nil, err
		}
		o.Port = p
	}
	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	n := &Node{URL: fmt.Sprintf("http://%s:%d", host, o.Port), Block: o.Block, Args: Args(o), exited: make(chan struct{})}
	n.cmd = exec.CommandContext(ctx, o.Anvil, n.Args...)
	out := o.Log
	if out == nil {
		out = io.Discard
	}
	n.cmd.Stdout, n.cmd.Stderr = out, out
	if err := n.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting anvil: %w", err)
	}
	var waitErr error
	go func() {
		waitErr = n.cmd.Wait()
		close(n.exited)
	}()
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-n.exited:
			return nil, fmt.Errorf("anvil exited before it was ready (%v); is the upstream endpoint reachable and does it serve block %d?", waitErr, o.Block)
		case <-ctx.Done():
			n.Stop()
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		if b, err := BlockNumber(ctx, n.URL); err == nil {
			if b != o.Block {
				n.Stop()
				return nil, fmt.Errorf("anvil came up at block %d, want %d", b, o.Block)
			}
			return n, nil
		}
		if time.Now().After(deadline) {
			n.Stop()
			return nil, fmt.Errorf("anvil did not answer within %s", wait)
		}
	}
}

// Stop kills the node and waits for it to exit. It is safe to call twice.
func (n *Node) Stop() {
	n.once.Do(func() {
		if n.cmd.Process != nil {
			n.cmd.Process.Kill()
		}
		select {
		case <-n.exited:
		case <-time.After(10 * time.Second):
		}
	})
}

// BlockNumber asks a node for eth_blockNumber with a short timeout. It is
// used as the readiness probe and to report the block of a running node.
func BlockNumber(ctx context.Context, url string) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var r struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	if r.Error != nil {
		return 0, errors.New(r.Error.Message)
	}
	return strconv.ParseUint(strings.TrimPrefix(r.Result, "0x"), 16, 64)
}
