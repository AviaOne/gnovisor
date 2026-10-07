// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AviaOne/gnovisor/internal/config"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/prepare"
	"github.com/AviaOne/gnovisor/internal/testutil"
)

// The fake node is this test binary started in helper mode by a small shell
// script that plays the gnoland binary of one version. It serves /status and
// /abci_query, and produces a block every 20ms while its version serves the
// next height, as a real node stops at a halt with its RPC open.

type control struct {
	Network        string              `json:"network"`
	HaltHeight     int64               `json:"halt_height"`
	HaltMinVersion string              `json:"halt_min_version"`
	Ranges         map[string][2]int64 `json:"ranges"` // version -> [from, to], to 0 = open
	Crash          bool                `json:"crash"`
}

func TestMain(m *testing.M) {
	if os.Getenv("GNOVISOR_FAKE_NODE") == "1" {
		os.Exit(fakeNode())
	}
	os.Exit(m.Run())
}

func readControl(path string) control {
	var c control
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &c)
	return c
}

func fakeNode() int {
	version := os.Getenv("FAKE_VERSION")
	ctrl := os.Getenv("FAKE_CONTROL")
	heightFile := os.Getenv("FAKE_HEIGHT")
	logFile := os.Getenv("FAKE_LOG")

	if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = fmt.Fprintf(f, "start %s GNOROOT=%s args=%s\n", version, os.Getenv("GNOROOT"), strings.Join(os.Args[1:], " "))
		_ = f.Close()
	}
	var mu sync.Mutex
	height := int64(0)
	if b, err := os.ReadFile(heightFile); err == nil {
		height, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	q := func(v string) string {
		d := "null"
		if v != "" {
			d = strconv.Quote(base64(v))
		}
		return `{"jsonrpc":"2.0","id":"","result":{"response":{"ResponseBase":{"Error":null,"Data":` + d + `,"Events":null,"Log":"","Info":""},"Key":null,"Value":null,"Proof":null,"Height":"0"}}}`
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		c := readControl(ctrl)
		mu.Lock()
		h := height
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"","result":{"node_info":{"network":%q},"sync_info":{"latest_block_height":"%d","catching_up":false}}}`, c.Network, h)
	})
	mux.HandleFunc("/abci_query", func(w http.ResponseWriter, r *http.Request) {
		c := readControl(ctrl)
		switch r.URL.Query().Get("path") {
		case `"params/node:p:halt_height"`:
			if c.HaltHeight == 0 {
				_, _ = w.Write([]byte(q("")))
				return
			}
			_, _ = w.Write([]byte(q(strconv.Quote(strconv.FormatInt(c.HaltHeight, 10)))))
		case `"params/node:p:halt_min_version"`:
			_, _ = w.Write([]byte(q(strconv.Quote(c.HaltMinVersion))))
		default:
			http.NotFound(w, r)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("FAKE_PORT"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake node:", err)
		return 2
	}
	go func() { _ = http.Serve(ln, mux) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(20 * time.Millisecond)
	for {
		select {
		case <-sig:
			return 0
		case <-tick.C:
			c := readControl(ctrl)
			if c.Crash {
				return 3
			}
			r, ok := c.Ranges[version]
			mu.Lock()
			next := height + 1
			if ok && next >= r[0] && (r[1] == 0 || next <= r[1]) {
				height = next
				_ = os.WriteFile(heightFile, []byte(strconv.FormatInt(height, 10)), 0o644)
			}
			mu.Unlock()
		}
	}
}

func base64(s string) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	b := []byte(s)
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		k := 0
		for j := 0; j < 3; j++ {
			n <<= 8
			if i+j < len(b) {
				n |= uint32(b[i+j])
				k++
			}
		}
		for j := 0; j < 4; j++ {
			if j <= k {
				out.WriteByte(enc[(n>>(18-6*uint(j)))&63])
			} else {
				out.WriteByte('=')
			}
		}
	}
	return out.String()
}

// harness is one supervised fake node.
type harness struct {
	t       *testing.T
	f       *testutil.Fixture
	lay     layout.Layout
	cfg     *config.Config
	ctrl    string
	height  string
	nodeLog string
	ledger  string

	mu   sync.Mutex
	logs []string

	cancel context.CancelFunc
	done   chan error
}

func script(version string) []byte {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return []byte(fmt.Sprintf("#!/bin/sh\nexec env GNOVISOR_FAKE_NODE=1 FAKE_VERSION=%s %q \"$@\"\n", version, exe))
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// newHarness: releases are all known to the fake node; the ledger GnoVisor
// reads holds the first `published` of them.
func newHarness(t *testing.T, published int, releases ...*testutil.Release) *harness {
	t.Helper()
	for _, r := range releases {
		r.Binary = script(r.Version)
	}
	h := &harness{t: t, f: testutil.NewFixture(t, "test-1", releases...)}
	h.lay = layout.New(t.TempDir())
	dir := t.TempDir()
	h.ctrl, h.height, h.nodeLog, h.ledger = filepath.Join(dir, "control.json"), filepath.Join(dir, "height"), filepath.Join(dir, "node.log"), filepath.Join(dir, "upgrades.json")
	port := freePort(t)
	t.Setenv("FAKE_CONTROL", h.ctrl)
	t.Setenv("FAKE_HEIGHT", h.height)
	t.Setenv("FAKE_LOG", h.nodeLog)
	t.Setenv("FAKE_PORT", port)

	// Ranges from the full list, as the chain will run them.
	ranges := map[string][2]int64{}
	full, err := ledger.Parse(h.f.Ledger(-1))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range full.BlockRanges() {
		ranges[r.Version] = [2]int64{r.From, r.To}
		for _, v := range r.Rolling {
			ranges[v] = [2]int64{r.From, r.To}
		}
	}
	h.setControl(func(c *control) { c.Network = "test-1"; c.Ranges = ranges })
	h.publish(published)

	if err := os.MkdirAll(filepath.Join(h.lay.Home, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(h.lay.Home, "secrets", "priv_validator_key.json"), []byte("KEY"), 0o600)
	_ = os.MkdirAll(filepath.Join(h.lay.Home, "db"), 0o755)
	_ = os.WriteFile(filepath.Join(h.lay.Home, "db", "data"), []byte("db"), 0o644)

	w, _ := config.ParseWindow("00:00-23:59")
	h.cfg = &config.Config{
		NodeDir: h.lay.Home, NodeArgs: []string{"--chainid", "test-1"}, ChainID: "test-1",
		RPC: "http://127.0.0.1:" + port, LedgerURL: h.ledger, GnorootRepo: h.f.Repo,
		PollInterval: 20 * time.Millisecond, AutoDownload: true, Backup: true,
		BackupDir: filepath.Join(h.lay.Root, "backups"), ShutdownGrace: 2 * time.Second,
		HaltConfirm: 200 * time.Millisecond, RestartTimeout: 3 * time.Second, RollingWindow: w,
	}
	// The genesis version, as `gnovisor init` leaves it.
	l, _ := ledger.Parse(h.f.Ledger(1))
	d, err := prepare.Version(context.Background(), h.lay, &l.Upgrades[0], prepare.Options{
		Platform: "linux/amd64", AutoDownload: true, GnorootRepo: h.f.Repo, HTTPClient: h.f.Client(), Registry: h.f.Server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.lay.Switch(d); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) setControl(fn func(*control)) {
	c := readControl(h.ctrl)
	fn(&c)
	b, _ := json.Marshal(c)
	if err := layout.WriteFileAtomic(h.ctrl, b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) publish(n int) {
	if err := layout.WriteFileAtomic(h.ledger, h.f.Ledger(n), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	s := &Supervisor{
		Cfg: h.cfg, Layout: h.lay, Platform: "linux/amd64", HTTPClient: h.f.Client(), Registry: h.f.Server.URL,
		Log: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
			h.mu.Unlock()
		},
	}
	go func() { h.done <- s.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Error("supervisor did not stop")
		}
	})
}

func (h *harness) logged(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (h *harness) chainHeight() int64 {
	b, _ := os.ReadFile(h.height)
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func (h *harness) nodeStarts() []string {
	b, _ := os.ReadFile(h.nodeLog)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, strings.Fields(l)[1])
		}
	}
	return out
}

func (h *harness) currentVersion() string {
	d, err := h.lay.CurrentDir()
	if err != nil {
		return ""
	}
	i, _ := layout.ReadInfo(d)
	return i.Version
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.t.Fatalf("timed out waiting for %s\nheight %d\nlogs:\n%s", what, h.chainHeight(), strings.Join(h.logs, "\n"))
}

func (h *harness) state() layout.State {
	s, _ := h.lay.ReadState()
	return s
}

func rel(kind, v string, halt int64) *testutil.Release {
	return &testutil.Release{Kind: kind, Version: v, HaltHeight: halt}
}

func TestScheduledHaltIsPreparedThenSwitched(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	// GovDAO has programmed the halt and the ledger publishes the version.
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("preparation", func() bool { return h.logged("ready for the halt at height 30, version v1.6.0") })
	h.waitFor("switch", func() bool { return h.chainHeight() >= 35 })

	if v := h.currentVersion(); v != "v1.6.0" {
		t.Fatalf("current = %s", v)
	}
	if st := h.state(); st.LastHalt != 30 || st.Pending != nil {
		t.Fatalf("state = %+v", st)
	}
	starts := h.nodeStarts()
	if len(starts) != 2 || starts[0] != "v1.5.0" || starts[1] != "v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	backup := filepath.Join(h.cfg.BackupDir, "30-v1.5.0")
	if _, err := os.Stat(filepath.Join(backup, "db", "data")); err != nil {
		t.Fatal("no backup of db")
	}
	if _, err := os.Stat(filepath.Join(backup, "secrets")); !os.IsNotExist(err) {
		t.Fatal("secrets/ must never be backed up")
	}
	b, _ := os.ReadFile(h.nodeLog)
	if !strings.Contains(string(b), "GNOROOT="+layout.Gnoroot(h.lay.Upgrade("v1.6.0"))) ||
		!strings.Contains(string(b), "args=start --data-dir "+h.lay.Home+" --chainid test-1") {
		t.Fatalf("node started with the wrong GNOROOT or arguments:\n%s", b)
	}
}

func TestMissingImageDigestDoesNotHoldTheNode(t *testing.T) {
	// The ledger lists the version before gno.land filled its image digest:
	// the image is read by its tag and the switch happens on time.
	up := rel("upgrade", "v1.6.0", 30)
	up.NoImageDigest = true
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), up)
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("switch without a digest", func() bool { return h.chainHeight() >= 32 })
	if h.currentVersion() != "v1.6.0" {
		t.Fatal("not switched")
	}
}

func TestLastMinuteVersionInTheLedgerIsTheOneRun(t *testing.T) {
	// v1.6.0 is prepared for the halt, then gno.land replaces it in the
	// ledger by v1.6.1 shortly before the halt. The ledger is read again at
	// the halt, even with an hour-long interval, and v1.6.1 runs.
	oldL := LedgerInterval
	LedgerInterval = time.Hour
	t.Cleanup(func() { LedgerInterval = oldL })

	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 200), rel("upgrade", "v1.6.1", 300))
	h.setControl(func(c *control) {
		c.HaltHeight = 200
		c.Ranges["v1.5.0"] = [2]int64{1, 200}
		c.Ranges["v1.6.0"] = [2]int64{201, 0}
		c.Ranges["v1.6.1"] = [2]int64{201, 0}
	})
	h.run()
	h.waitFor("v1.6.0 prepared", func() bool { return h.logged("ready for the halt at height 200, version v1.6.0") })
	// The replacement ledger: v1.6.1 activated by the halt at 200, no v1.6.0.
	var doc map[string]any
	if err := json.Unmarshal(h.f.Ledger(3), &doc); err != nil {
		t.Fatal(err)
	}
	ups := doc["upgrades"].([]any)
	last := ups[2].(map[string]any)
	last["halt_height"] = 200
	doc["upgrades"] = []any{ups[0], last}
	b, _ := json.Marshal(doc)
	if err := layout.WriteFileAtomic(h.ledger, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.waitFor("switch", func() bool { return h.chainHeight() >= 202 })
	if v := h.currentVersion(); v != "v1.6.1" {
		t.Fatalf("current = %s, want the version the ledger names at the halt", v)
	}
}

func TestKnownHaltIsHandledWithoutWaitingForHaltConfirm(t *testing.T) {
	// G12: at the height of a halt the chain or the ledger names, block H is
	// committed and the node will produce nothing more; waiting halt_confirm
	// only lengthens the outage.
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.cfg.HaltConfirm = time.Hour
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("switch without waiting an hour", func() bool { return h.chainHeight() >= 32 })
	if h.currentVersion() != "v1.6.0" {
		t.Fatal("not switched")
	}
}

func TestMissingVersionKeepsTheNodeStoppedAndNeverRestartsTheOldBinary(t *testing.T) {
	old := PendingLedgerInterval
	PendingLedgerInterval = 300 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("pending halt", func() bool { s := h.state(); return s.Pending != nil && s.Pending.Height == 30 })
	time.Sleep(time.Second)
	if starts := h.nodeStarts(); len(starts) != 1 {
		t.Fatalf("the old binary was started again: %v", starts)
	}
	if h.currentVersion() != "v1.5.0" {
		t.Fatal("no switch without a version")
	}
	h.publish(2)
	h.waitFor("switch once published", func() bool { return h.chainHeight() >= 32 })
	if st := h.state(); st.Pending != nil || st.LastHalt != 30 {
		t.Fatalf("state = %+v", st)
	}
}

func TestMissingVersionIsLookedForEveryPendingInterval(t *testing.T) {
	oldL, oldP := LedgerInterval, PendingLedgerInterval
	LedgerInterval, PendingLedgerInterval = time.Hour, 200*time.Millisecond
	t.Cleanup(func() { LedgerInterval, PendingLedgerInterval = oldL, oldP })

	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("pending halt", func() bool { s := h.state(); return s.Pending != nil && s.Pending.Height == 30 })
	h.publish(2)
	// With the normal hour-long interval the version would not be seen.
	h.waitFor("switch within the pending interval", func() bool { return h.chainHeight() >= 32 })
}

func TestPendingHaltSurvivesARestartOfGnoVisor(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	if err := h.lay.WriteState(layout.State{Pending: &layout.Pending{Height: 30}}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(h.height, []byte("30"), 0o644)
	h.publish(2)
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
	if starts := h.nodeStarts(); len(starts) != 1 || starts[0] != "v1.6.0" {
		t.Fatalf("with a pending halt the old binary must never start: %v", starts)
	}
}

func TestSyncFromGenesisCrossesEveryPastHalt(t *testing.T) {
	h := newHarness(t, 3, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 20), rel("upgrade", "v1.7.0", 40))
	h.cfg.Backup = false
	// The chain parameter carries the last halt, as on mainnet today
	// (halt_height 162200 long after it happened).
	h.setControl(func(c *control) { c.HaltHeight = 40 })
	h.run()
	h.waitFor("synced past both halts", func() bool { return h.chainHeight() >= 45 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0,v1.7.0" {
		t.Fatalf("node runs = %v", starts)
	}
	if st := h.state(); st.LastHalt != 40 {
		t.Fatalf("state = %+v", st)
	}
}

func TestLocalHaltIsNotAnUpgrade(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	// The node stops at 12 by itself (a halt_height in its config.toml).
	h.setControl(func(c *control) { c.Ranges["v1.5.0"] = [2]int64{1, 12} })
	h.run()
	h.waitFor("warning", func() bool { return h.logged("neither the chain halt nor a ledger halt") })
	time.Sleep(300 * time.Millisecond)
	if h.currentVersion() != "v1.5.0" || len(h.nodeStarts()) != 1 {
		t.Fatal("a local halt must change nothing")
	}
}

func TestFailedRestartLeavesTheNodeStoppedWithoutGoingBack(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.cfg.RestartTimeout = 500 * time.Millisecond
	// v1.6.0 refuses to produce block 31, as a node refused by its floor.
	h.setControl(func(c *control) { c.HaltHeight = 30; c.Ranges["v1.6.0"] = [2]int64{50, 0} })
	h.run()
	h.waitFor("failure", func() bool { return h.logged("no return to the old version") })
	h.waitFor("watch for a fix", func() bool { return h.logged("waiting for a version other than v1.6.0") })
	if h.currentVersion() != "v1.6.0" {
		t.Fatal("must not go back to v1.5.0")
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	if st := h.state(); st.Pending == nil || st.Pending.Height != 30 {
		t.Fatalf("the halt must stay pending so a restart resumes the watch: %+v", st)
	}
}

func TestFixPublishedAfterAFailedVersionIsTaken(t *testing.T) {
	// v1.6.0 runs after the halt but produces no block (an app hash
	// mismatch); gno.land publishes v1.6.1 and names it in the ledger for
	// the same halt. GnoVisor, still watching, switches to it.
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30), rel("upgrade", "v1.6.1", 300))
	h.cfg.RestartTimeout = 500 * time.Millisecond
	h.cfg.Backup = false
	h.setControl(func(c *control) {
		c.HaltHeight = 30
		c.Ranges["v1.5.0"] = [2]int64{1, 30}
		c.Ranges["v1.6.0"] = [2]int64{1000, 0} // never produces block 31
		c.Ranges["v1.6.1"] = [2]int64{31, 0}
	})
	h.run()
	h.waitFor("watch for a fix", func() bool { return h.logged("waiting for a version other than v1.6.0") })
	var doc map[string]any
	if err := json.Unmarshal(h.f.Ledger(3), &doc); err != nil {
		t.Fatal(err)
	}
	ups := doc["upgrades"].([]any)
	fix := ups[2].(map[string]any)
	fix["halt_height"] = 30
	doc["upgrades"] = []any{ups[0], fix}
	b, _ := json.Marshal(doc)
	if err := layout.WriteFileAtomic(h.ledger, b, 0o644); err != nil {
		t.Fatal(err)
	}
	h.waitFor("switch to the fix", func() bool { return h.chainHeight() >= 32 })
	if v := h.currentVersion(); v != "v1.6.1" {
		t.Fatalf("current = %s", v)
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0,v1.6.1" {
		t.Fatalf("node runs = %v", starts)
	}
	if st := h.state(); st.Pending != nil || st.LastHalt != 30 {
		t.Fatalf("state = %+v", st)
	}
}

func TestNodeExitingByItselfStopsGnoVisorWithAnError(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.run()
	h.waitFor("blocks", func() bool { return h.chainHeight() >= 3 })
	h.setControl(func(c *control) { c.Crash = true })
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Fatalf("err = %v", err)
		}
		h.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("gnovisor did not stop")
	}
	if len(h.nodeStarts()) != 1 {
		t.Fatal("gnovisor must not restart a crashed node itself")
	}
}

func TestRollingReleaseWaitsForTheWindow(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("rolling", "v1.5.1", 0))
	now := time.Now().UTC()
	closed := fmt.Sprintf("%02d:00-%02d:00", (now.Hour()+2)%24, (now.Hour()+3)%24)
	h.cfg.RollingWindow, _ = config.ParseWindow(closed)
	h.run()
	h.waitFor("prepared", func() bool { return h.logged("rolling release v1.5.1 ready") })
	time.Sleep(300 * time.Millisecond)
	if h.currentVersion() != "v1.5.0" {
		t.Fatal("installed outside the window")
	}
}

func TestRollingReleaseInsideTheWindow(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("rolling", "v1.5.1", 0))
	h.run()
	h.waitFor("installed", func() bool { return h.currentVersion() == "v1.5.1" })
	start := h.chainHeight()
	h.waitFor("blocks after", func() bool { return h.chainHeight() > start+3 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.5.1" {
		t.Fatalf("node runs = %v", starts)
	}
}

func TestNoRollingWhileAHaltIsProgrammed(t *testing.T) {
	h := newHarness(t, 3, rel("genesis", "v1.5.0", 0), rel("rolling", "v1.5.1", 0), rel("upgrade", "v1.6.0", 1000))
	h.setControl(func(c *control) { c.HaltHeight = 1000 })
	h.run()
	h.waitFor("halt prepared", func() bool { return h.logged("ready for the halt at height 1000") })
	time.Sleep(300 * time.Millisecond)
	if h.currentVersion() != "v1.5.0" {
		t.Fatal("a rolling release must not be installed while a halt is programmed")
	}
}

func TestBackupFailureBlocksTheSwitch(t *testing.T) {
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	// A backup that already exists cannot be written.
	if err := os.MkdirAll(filepath.Join(h.cfg.BackupDir, "30-v1.5.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("backup failure", func() bool { return h.logged("backup failed, no switch") })
	if h.currentVersion() != "v1.5.0" {
		t.Fatal("no switch without the requested backup")
	}
	if st := h.state(); st.Pending == nil || st.Pending.Height != 30 {
		t.Fatalf("the halt must stay pending so a restart never starts the old binary: %+v", st)
	}
}

func TestWrongChainIsRefused(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.setControl(func(c *control) { c.Network = "other-1" })
	h.run()
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "other-1") {
			t.Fatalf("err = %v", err)
		}
		h.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("gnovisor did not refuse")
	}
}

// addLocal does what gnovisor add-upgrade leaves on disk.
func (h *harness) addLocal(version string, halt int64, now bool) {
	h.t.Helper()
	dir := h.lay.Upgrade(version)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(layout.Gnoroot(dir), "gnovm", "stdlibs"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(layout.Binary(dir), script(version), 0o755); err != nil {
		h.t.Fatal(err)
	}
	sum, _ := layout.SHA256File(layout.Binary(dir))
	if err := layout.WriteInfo(dir, layout.Info{Version: version, SHA256: sum}); err != nil {
		h.t.Fatal(err)
	}
	f, _ := local.Read(h.lay)
	f.Upgrades = append(f.Upgrades, local.Entry{Version: version, HaltHeight: halt, Now: now, SHA256: sum, Added: time.Now()})
	if err := local.Write(h.lay, f); err != nil {
		h.t.Fatal(err)
	}
}

func TestVersionAddedByHandDuringThePendingHalt(t *testing.T) {
	// The private security fix: GovDAO halts the chain, the binary never
	// reaches the public ledger, the operator adds it while the node waits.
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.setControl(func(c *control) {
		c.HaltHeight = 30
		c.Ranges["v1.5.0"] = [2]int64{1, 30}
		c.Ranges["v1.5.9"] = [2]int64{31, 0}
	})
	h.run()
	h.waitFor("pending halt", func() bool { s := h.state(); return s.Pending != nil && s.Pending.Height == 30 })
	h.addLocal("v1.5.9", 30, false)
	h.waitFor("switch to the version added by hand", func() bool { return h.chainHeight() >= 32 })
	if h.currentVersion() != "v1.5.9" || !h.logged("using version v1.5.9 added by hand") {
		t.Fatalf("current = %s", h.currentVersion())
	}
}

func TestVersionAddedByHandBeforeTheHaltIsReadyAhead(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.setControl(func(c *control) {
		c.HaltHeight = 30
		c.Ranges["v1.5.0"] = [2]int64{1, 30}
		c.Ranges["v1.5.9"] = [2]int64{31, 0}
	})
	h.addLocal("v1.5.9", 30, false)
	h.run()
	h.waitFor("ready ahead", func() bool { return h.logged("ready for the halt at height 30, version v1.5.9 added by hand") })
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
}

func TestLedgerAndHandDisagreeingBlocksTheSwitch(t *testing.T) {
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.setControl(func(c *control) { c.HaltHeight = 30; c.Ranges["v1.6.5"] = [2]int64{31, 0} })
	h.addLocal("v1.6.5", 30, false)
	h.run()
	h.waitFor("conflict reported", func() bool { return h.logged("the ledger gives v1.6.0") })
	time.Sleep(500 * time.Millisecond)
	if h.currentVersion() != "v1.5.0" || len(h.nodeStarts()) != 1 {
		t.Fatalf("no switch on a conflict: current %s, runs %v", h.currentVersion(), h.nodeStarts())
	}
	if s := h.state(); s.Pending == nil || s.Pending.Height != 30 {
		t.Fatalf("the halt must stay pending: %+v", s)
	}
}

func TestRollingAddedByHandWithNowIgnoresTheWindow(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	now := time.Now().UTC()
	h.cfg.RollingWindow, _ = config.ParseWindow(fmt.Sprintf("%02d:00-%02d:00", (now.Hour()+2)%24, (now.Hour()+3)%24))
	h.setControl(func(c *control) { c.Ranges["v1.5.1"] = [2]int64{1, 0} })
	h.run()
	h.waitFor("blocks", func() bool { return h.chainHeight() >= 3 })
	h.addLocal("v1.5.1", 0, true)
	h.waitFor("installed at once", func() bool { return h.currentVersion() == "v1.5.1" })
	start := h.chainHeight()
	h.waitFor("blocks after", func() bool { return h.chainHeight() > start+2 })
}

func TestRollingAddedByHandWaitsForTheWindow(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	now := time.Now().UTC()
	h.cfg.RollingWindow, _ = config.ParseWindow(fmt.Sprintf("%02d:00-%02d:00", (now.Hour()+2)%24, (now.Hour()+3)%24))
	h.setControl(func(c *control) { c.Ranges["v1.5.1"] = [2]int64{1, 0} })
	h.addLocal("v1.5.1", 0, false)
	h.run()
	h.waitFor("blocks", func() bool { return h.chainHeight() >= 5 })
	time.Sleep(300 * time.Millisecond)
	if h.currentVersion() != "v1.5.0" {
		t.Fatal("installed outside the window")
	}
}
