// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package rpc reads what GnoVisor needs from a gno.land node's JSON-RPC.
//
// Shapes, from gnolang/gno at commit 156777e0:
//   - every result is Amino JSON: int64 values are quoted strings, byte
//     arrays are base64 (docs/resources/rpc-endpoints.md, "Reading a
//     response");
//   - /status returns node_info.network and sync_info.latest_block_height
//     (tm2/pkg/bft/rpc/core/types/responses.go, ResultStatus);
//   - abci_query returns response.ResponseBase.{Error,Data}; a failed query
//     is still HTTP 200 with the failure in ResponseBase.Error
//     (docs/resources/rpc-endpoints.md, abci_query);
//   - path "params/<key>" puts the stored Amino JSON of the parameter in
//     Data, nothing when the key was never set
//     (tm2/pkg/sdk/params/handler.go, Query).
package rpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Parameter keys written by a GovDAO halt proposal
// (gno.land/adr/pr5368_govdao_halt_height.md).
const (
	KeyHaltHeight     = "node:p:halt_height"
	KeyHaltMinVersion = "node:p:halt_min_version"
)

// Client talks to one node.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for base, such as "http://127.0.0.1:26657".
func New(base string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

// Status is the part of /status GnoVisor reads.
type Status struct {
	Network    string
	Height     int64
	CatchingUp bool
}

// Status queries /status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var res struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
		SyncInfo struct {
			LatestBlockHeight aminoInt `json:"latest_block_height"`
			CatchingUp        bool     `json:"catching_up"`
		} `json:"sync_info"`
	}
	if err := c.call(ctx, "status", nil, &res); err != nil {
		return Status{}, err
	}
	return Status{Network: res.NodeInfo.Network, Height: int64(res.SyncInfo.LatestBlockHeight), CatchingUp: res.SyncInfo.CatchingUp}, nil
}

// Halt is the coordinated halt the chain currently carries.
type Halt struct {
	Height     int64  // 0: none
	MinVersion string // "": no floor
}

// Halt reads both halt parameters.
func (c *Client) Halt(ctx context.Context) (Halt, error) {
	var h Halt
	raw, err := c.param(ctx, KeyHaltHeight)
	if err != nil {
		return h, err
	}
	if raw != nil {
		var v aminoInt
		if err := json.Unmarshal(raw, &v); err != nil {
			return h, fmt.Errorf("%s: %w", KeyHaltHeight, err)
		}
		h.Height = int64(v)
	}
	raw, err = c.param(ctx, KeyHaltMinVersion)
	if err != nil {
		return h, err
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &h.MinVersion); err != nil {
			return h, fmt.Errorf("%s: %w", KeyHaltMinVersion, err)
		}
	}
	return h, nil
}

// param returns the Amino JSON stored under key, or nil if never set.
func (c *Client) param(ctx context.Context, key string) (json.RawMessage, error) {
	var res struct {
		Response struct {
			ResponseBase struct {
				Error json.RawMessage `json:"Error"`
				Data  *string         `json:"Data"`
				Log   string          `json:"Log"`
			} `json:"ResponseBase"`
		} `json:"response"`
	}
	q := url.Values{"path": {strconv.Quote("params/" + key)}}
	if err := c.call(ctx, "abci_query", q, &res); err != nil {
		return nil, err
	}
	rb := res.Response.ResponseBase
	if len(rb.Error) > 0 && string(rb.Error) != "null" {
		return nil, fmt.Errorf("abci_query %s: %s %s", key, rb.Error, rb.Log)
	}
	if rb.Data == nil || *rb.Data == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(*rb.Data)
	if err != nil {
		return nil, fmt.Errorf("abci_query %s: %w", key, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	return b, nil
}

func (c *Client) call(ctx context.Context, method string, q url.Values, out any) error {
	u := c.base + "/" + method
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", method, resp.StatusCode)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: RPC error %d %s %s", method, env.Error.Code, env.Error.Message, env.Error.Data)
	}
	if len(env.Result) == 0 {
		return errors.New(method + ": empty result")
	}
	return json.Unmarshal(env.Result, out)
}

// aminoInt reads an int64 written by Amino JSON as a quoted string, and also
// accepts a bare number.
type aminoInt int64

func (a *aminoInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not an integer: %s", b)
	}
	*a = aminoInt(n)
	return nil
}
