// Package signalcli runs signal-cli's daemon and talks to it: JSON-RPC calls over HTTP, and the stream of incoming
// envelopes over SSE.
package signalcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// RPCError is signal-cli refusing a call, with its reason. Its messages are meant for users.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return e.Message
}

// Client calls the daemon's JSON-RPC API.
type Client struct {
	base string
	http *http.Client
	ids  atomic.Int64
}

func NewClient(baseURL string) *Client {
	// Sends wait for every recipient's server round trip, and attachments are uploaded within the call.
	return &Client{base: baseURL, http: &http.Client{Timeout: 5 * time.Minute}}
}

// Up reports whether the daemon is answering.
func (c *Client) Up(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/check", nil)
	if err != nil {
		return false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// Call runs method with params (a struct, marshalled as the call's named parameters) and decodes its result into
// result, unless result is nil.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if params == nil {
		params = struct{}{}
	}
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: c.ids.Add(1), Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/rpc", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("signal-cli %s: %w", method, err)
	}
	defer resp.Body.Close()
	var decoded rpcResponse
	if err = json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("signal-cli %s: reading response (HTTP %d): %w", method, resp.StatusCode, err)
	}
	if decoded.Error != nil {
		return decoded.Error
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signal-cli %s: HTTP %d", method, resp.StatusCode)
	}
	if result == nil {
		return nil
	}
	if err = json.Unmarshal(decoded.Result, result); err != nil {
		return fmt.Errorf("signal-cli %s: decoding result: %w", method, err)
	}
	return nil
}

// IsRPCError reports whether err is signal-cli refusing a call (as opposed to failing to reach it).
func IsRPCError(err error) bool {
	return errors.As(err, new(*RPCError))
}
