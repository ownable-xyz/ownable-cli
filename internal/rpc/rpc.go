// SPDX-License-Identifier: Apache-2.0
// Copyright (c) wattsy

// Package rpc is a minimal Ethereum JSON-RPC client: eth_call with an explicit
// gas limit, tolerant of large responses, and able to tell an execution
// failure (the call did not fit, or reverted) from a transport fault (retried).
package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ownable-xyz/ownable-cli/internal/hexutil"
)

// RevertError is an execution failure reported by the node: out of gas, a
// revert, an invalid opcode. Callers treat it as "this call did not fit or
// failed", never as a network problem.
type RevertError struct {
	Code    int
	Message string
	Data    string
}

func (e *RevertError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("execution failed: %s (data %s)", e.Message, e.Data)
	}
	return fmt.Sprintf("execution failed: %s", e.Message)
}

// OutOfGas reports whether the node said the call ran out of gas, as opposed
// to a revert with a reason.
func (e *RevertError) OutOfGas() bool {
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "out of gas") || strings.Contains(m, "gas required exceeds") ||
		strings.Contains(m, "gas limit") || strings.Contains(m, "allowance")
}

// Client is a single-endpoint JSON-RPC client. It is safe for concurrent use.
type Client struct {
	URL    string
	HTTP   *http.Client
	nextID atomic.Int64
	// Requests counts every JSON-RPC request sent (including retries).
	Requests atomic.Int64
	// Throttled counts rate-limit answers that were waited out.
	Throttled atomic.Int64

	// Pacing: after a rate-limit answer the client spaces all its requests
	// out (the gap doubles on every throttled answer and shrinks slowly on
	// success), so parallel workers back off together instead of each
	// hammering the endpoint.
	paceMu sync.Mutex
	gap    time.Duration
	next   time.Time
}

const (
	minGap = 20 * time.Millisecond
	maxGap = 2 * time.Second
)

// pace blocks until this request may be sent.
func (c *Client) pace(ctx context.Context) error {
	c.paceMu.Lock()
	if c.gap == 0 {
		c.paceMu.Unlock()
		return nil
	}
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.gap)
	c.paceMu.Unlock()
	if d := time.Until(at); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

func (c *Client) paceThrottled() {
	c.paceMu.Lock()
	c.gap = min(max(c.gap*2, minGap), maxGap)
	c.paceMu.Unlock()
}

func (c *Client) paceOK() {
	c.paceMu.Lock()
	if c.gap > 0 {
		c.gap = c.gap * 97 / 100
		if c.gap < minGap/2 {
			c.gap = 0
		}
	}
	c.paceMu.Unlock()
}

// New returns a client for one endpoint.
func New(url string) *Client {
	return &Client{
		URL:  url,
		HTTP: &http.Client{Timeout: 10 * time.Minute},
	}
}

type rpcReq struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int64         `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcErr         `json:"error"`
}

const (
	maxFaults    = 6                // transport faults and 5xx before giving up
	maxThrottled = 120              // rate-limit answers before giving up
	maxBackoff   = 30 * time.Second // longest wait between rate-limited attempts
)

// call performs one JSON-RPC request. Transport faults and 5xx are retried a
// few times; rate limiting (HTTP 429 or a rate-limit error) is waited out
// with growing backoff, honouring Retry-After. JSON-RPC errors are returned
// as RevertError.
func (c *Client) call(ctx context.Context, method string, params ...interface{}) (json.RawMessage, error) {
	body, err := json.Marshal(rpcReq{JSONRPC: "2.0", ID: c.nextID.Add(1), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	var (
		lastErr          error
		faults, throttle int
		wait             time.Duration
	)
	for {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		fault := func(err error) {
			lastErr = err
			faults++
			wait = time.Duration(faults*faults) * 500 * time.Millisecond
		}
		throttled := func(err error, retryAfter string) {
			lastErr = err
			throttle++
			c.Throttled.Add(1)
			c.paceThrottled()
			wait = min(time.Duration(1<<min(throttle, 6))*250*time.Millisecond, maxBackoff)
			if secs, perr := strconv.Atoi(strings.TrimSpace(retryAfter)); perr == nil && secs > 0 {
				wait = min(time.Duration(secs)*time.Second, maxBackoff)
			}
			wait += time.Duration(rand.Int64N(int64(wait/4) + 1)) // jitter, so parallel workers spread out
		}
		if faults >= maxFaults || throttle >= maxThrottled {
			return nil, fmt.Errorf("rpc: %s failed after %d attempts: %w", method, faults+throttle, lastErr)
		}
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.Requests.Add(1)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			fault(redact(err, c.URL))
			continue
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fault(redact(err, c.URL))
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled(fmt.Errorf("rpc: HTTP 429: %s", truncate(string(raw), 200)), resp.Header.Get("Retry-After"))
			continue
		}
		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("rpc: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
			if resp.StatusCode >= 500 {
				fault(err)
				continue
			}
			return nil, err // other 4xx (request too large, bad request) will not heal
		}
		var r rpcResp
		if err := json.Unmarshal(raw, &r); err != nil {
			fault(fmt.Errorf("rpc: bad response: %w (%s)", err, truncate(string(raw), 120)))
			continue
		}
		if r.Error != nil {
			if isRateLimit(r.Error) {
				throttled(fmt.Errorf("rpc: rate limited: %s", r.Error.Message), "")
				continue
			}
			data := ""
			if len(r.Error.Data) > 0 {
				data = truncate(strings.Trim(string(r.Error.Data), `"`), 200)
			}
			c.paceOK()
			return nil, &RevertError{Code: r.Error.Code, Message: r.Error.Message, Data: data}
		}
		c.paceOK()
		return r.Result, nil
	}
}

func isRateLimit(e *rpcErr) bool {
	m := strings.ToLower(e.Message)
	return e.Code == 429 || e.Code == -32005 || strings.Contains(m, "rate limit") || strings.Contains(m, "too many requests") ||
		strings.Contains(m, "exceeded") && strings.Contains(m, "capacity")
}

// redact keeps an endpoint URL (which often embeds an API key) out of errors.
func redact(err error, url string) error {
	if url == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), url, "<rpc>"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func callObj(to string, data []byte, gas uint64) map[string]string {
	obj := map[string]string{"to": to, "data": hexutil.Encode(data)}
	if gas > 0 {
		obj["gas"] = "0x" + strconv.FormatUint(gas, 16)
	}
	return obj
}

// EthCall performs eth_call(to, data) at the given block tag with an explicit
// gas limit. A RevertError means the call failed in the EVM; anything else is
// a transport failure.
func (c *Client) EthCall(ctx context.Context, to string, data []byte, gas uint64, block string) ([]byte, error) {
	res, err := c.call(ctx, "eth_call", callObj(to, data, gas), block)
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(res, &hexStr); err != nil {
		return nil, fmt.Errorf("rpc: eth_call result: %w", err)
	}
	return hexutil.Decode(hexStr)
}

func (c *Client) quantity(ctx context.Context, method string, params ...interface{}) (uint64, error) {
	res, err := c.call(ctx, method, params...)
	if err != nil {
		return 0, err
	}
	var hexStr string
	if err := json.Unmarshal(res, &hexStr); err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(hexStr, "0x"), 16, 64)
}

// ChainID reads eth_chainId.
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_chainId")
}

// BlockNumber reads eth_blockNumber.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_blockNumber")
}

// Code reads eth_getCode at the given block tag.
func (c *Client) Code(ctx context.Context, addr, block string) ([]byte, error) {
	res, err := c.call(ctx, "eth_getCode", addr, block)
	if err != nil {
		return nil, err
	}
	var hexStr string
	if err := json.Unmarshal(res, &hexStr); err != nil {
		return nil, err
	}
	return hexutil.Decode(hexStr)
}

// EstimateGas measures a call with eth_estimateGas, bounded by gas. The node
// runs the call (usually twice); it never sends anything.
func (c *Client) EstimateGas(ctx context.Context, to string, data []byte, gas uint64, block string) (uint64, error) {
	return c.quantity(ctx, "eth_estimateGas", callObj(to, data, gas), block)
}

// TraceCallGas measures a call's gas with debug_traceCall (the callTracer's
// top frame). It returns ok=false when the node has no debug namespace.
func (c *Client) TraceCallGas(ctx context.Context, to string, data []byte, gas uint64, block string) (uint64, bool) {
	// onlyTopCall keeps nested frames (and their copies of the input) out of
	// the response; the top frame's gasUsed is all that is read.
	res, err := c.call(ctx, "debug_traceCall", callObj(to, data, gas), block,
		map[string]any{"tracer": "callTracer", "tracerConfig": map[string]any{"onlyTopCall": true}})
	if err != nil {
		debugf("debug_traceCall: %v", err)
		return 0, false
	}
	var frame struct {
		GasUsed string `json:"gasUsed"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(res, &frame); err != nil || frame.Error != "" || frame.GasUsed == "" {
		debugf("debug_traceCall: unusable frame (error %q, gasUsed %q)", frame.Error, frame.GasUsed)
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(frame.GasUsed, "0x"), 16, 64)
	return v, err == nil
}
