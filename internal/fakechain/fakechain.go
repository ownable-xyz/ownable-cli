// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package fakechain is a JSON-RPC server for tests. It answers eth_call,
// eth_estimateGas and debug_traceCall from Go handlers, one per contract
// address, with a gas cap enforced like a node does: a handler reports the gas
// a call needs, and a call that needs more than its gas limit fails with
// "out of gas".
package fakechain

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Handler executes one call: it returns the return data and the gas used, or
// an error to report as a revert.
type Handler func(data []byte) (ret []byte, gas uint64, err error)

// Chain is a fake node.
type Chain struct {
	Server   *httptest.Server
	ChainID  uint64
	NoTrace  bool   // answer debug_traceCall with "method not found"
	EstCap   uint64 // if set, eth_estimateGas fails above this gas, like nodes that cap estimates
	mu       sync.Mutex
	handlers map[string]Handler
	Code     map[string][]byte
	Calls    map[string]int
}

// New starts a fake node.
func New() *Chain {
	c := &Chain{ChainID: 1, handlers: map[string]Handler{}, Code: map[string][]byte{}, Calls: map[string]int{}}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

// Close stops the server.
func (c *Chain) Close() { c.Server.Close() }

// URL is the endpoint.
func (c *Chain) URL() string { return c.Server.URL }

// Handle registers a contract.
func (c *Chain) Handle(addr string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[strings.ToLower(addr)] = h
	c.Code[strings.ToLower(addr)] = []byte{0x60, 0x00}
}

type req struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type callArgs struct {
	To   string `json:"to"`
	Data string `json:"data"`
	Gas  string `json:"gas"`
}

func (c *Chain) serve(w http.ResponseWriter, r *http.Request) {
	var q req
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	result, rerr := c.dispatch(&q)
	resp := map[string]any{"jsonrpc": "2.0", "id": q.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func hexq(v uint64) string { return "0x" + strconv.FormatUint(v, 16) }

func (c *Chain) dispatch(q *req) (any, *rpcError) {
	switch q.Method {
	case "eth_chainId":
		return hexq(c.ChainID), nil
	case "eth_blockNumber":
		return hexq(100), nil
	case "eth_getCode":
		var addr string
		json.Unmarshal(q.Params[0], &addr)
		c.mu.Lock()
		code := c.Code[strings.ToLower(addr)]
		c.mu.Unlock()
		return "0x" + hex.EncodeToString(code), nil
	case "eth_call", "eth_estimateGas", "debug_traceCall":
		if q.Method == "debug_traceCall" && c.NoTrace {
			return nil, &rpcError{Code: -32601, Message: "the method debug_traceCall does not exist/is not available"}
		}
		var a callArgs
		if err := json.Unmarshal(q.Params[0], &a); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		data, err := hex.DecodeString(strings.TrimPrefix(a.Data, "0x"))
		if err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		limit := uint64(50_000_000)
		if a.Gas != "" {
			limit, _ = strconv.ParseUint(strings.TrimPrefix(a.Gas, "0x"), 16, 64)
		}
		c.mu.Lock()
		h := c.handlers[strings.ToLower(a.To)]
		if q.Method == "eth_call" {
			c.Calls[strings.ToLower(a.To)]++
		}
		c.mu.Unlock()
		if h == nil {
			return "0x", nil // a call to an account without code succeeds with no data
		}
		ret, gas, herr := h(data)
		if herr != nil {
			return nil, &rpcError{Code: 3, Message: "execution reverted: " + herr.Error()}
		}
		if q.Method == "eth_estimateGas" && c.EstCap > 0 && gas > c.EstCap {
			return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("gas required exceeds allowance (%d)", c.EstCap)}
		}
		if gas > limit {
			if q.Method == "eth_call" {
				return nil, &rpcError{Code: -32000, Message: "out of gas"}
			}
			return nil, &rpcError{Code: -32000, Message: fmt.Sprintf("gas required exceeds allowance (%d)", limit)}
		}
		switch q.Method {
		case "eth_estimateGas":
			return hexq(gas), nil
		case "debug_traceCall":
			return map[string]string{"gasUsed": hexq(gas)}, nil
		}
		return "0x" + hex.EncodeToString(ret), nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found"}
}
