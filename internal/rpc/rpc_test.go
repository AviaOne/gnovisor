// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package rpc

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The /status body reuses the values read on rpc.gno.land on 2026-10-05
// (source of truth, 3.2).
const statusBody = `{"jsonrpc":"2.0","id":"","result":{"node_info":{"network":"gnoland-1","version":"x"},` +
	`"sync_info":{"latest_block_hash":"AA==","latest_app_hash":"AA==","latest_block_height":"551853",` +
	`"latest_block_time":"2026-10-04T12:24:17Z","catching_up":false},` +
	`"validator_info":{"address":"g1x","pub_key":null,"voting_power":"0"},` +
	`"build_version":"heads/chain/mainnet.3444+e75fef82c"}}`

func query(data *string, errJSON string) string {
	d := "null"
	if data != nil {
		d = `"` + *data + `"`
	}
	if errJSON == "" {
		errJSON = "null"
	}
	return `{"jsonrpc":"2.0","id":"","result":{"response":{"ResponseBase":{"Error":` + errJSON +
		`,"Data":` + d + `,"Events":null,"Log":"","Info":""},"Key":null,"Value":null,"Proof":null,"Height":"0"}}}`
}

func b64(s string) *string { v := base64.StdEncoding.EncodeToString([]byte(s)); return &v }

func server(t *testing.T, routes map[string]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if p := r.URL.Query().Get("path"); p != "" {
			key += "?" + p
		}
		body, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestStatusReadsQuotedHeight(t *testing.T) {
	c := server(t, map[string]string{"/status": statusBody})
	s, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Network != "gnoland-1" || s.Height != 551853 || s.CatchingUp {
		t.Fatalf("%+v", s)
	}
}

func TestHaltReadsAminoParams(t *testing.T) {
	// Values read on rpc.gno.land on 2026-10-05: "162200" and "".
	c := server(t, map[string]string{
		`/abci_query?"params/node:p:halt_height"`:      query(b64(`"162200"`), ""),
		`/abci_query?"params/node:p:halt_min_version"`: query(b64(`""`), ""),
	})
	h, err := c.Halt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Height != 162200 || h.MinVersion != "" {
		t.Fatalf("%+v", h)
	}
}

func TestHaltNeverSetIsZero(t *testing.T) {
	c := server(t, map[string]string{
		`/abci_query?"params/node:p:halt_height"`:      query(nil, ""),
		`/abci_query?"params/node:p:halt_min_version"`: query(b64(`"v1.6.0"`), ""),
	})
	h, err := c.Halt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Height != 0 || h.MinVersion != "v1.6.0" {
		t.Fatalf("%+v", h)
	}
}

func TestQueryErrorIsAnError(t *testing.T) {
	// A failed query is HTTP 200 with the error inside ResponseBase.
	c := server(t, map[string]string{
		`/abci_query?"params/node:p:halt_height"`: query(nil, `{"@type":"/std.UnknownRequestError"}`),
	})
	if _, err := c.Halt(context.Background()); err == nil {
		t.Fatal("an error in ResponseBase must not read as an unset parameter")
	}
}

func TestRPCErrorAndHTTPError(t *testing.T) {
	c := server(t, map[string]string{"/status": `{"jsonrpc":"2.0","id":"","error":{"code":-32603,"message":"x"}}`})
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("RPC error accepted")
	}
	c = server(t, map[string]string{})
	if _, err := c.Status(context.Background()); err == nil {
		t.Fatal("HTTP 404 accepted")
	}
}
