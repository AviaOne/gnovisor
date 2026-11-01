// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/local"
)

// Since 2026-10-29 gno.land writes the ledger entry of a coordinated upgrade
// only once the chain runs the release, after the halt
// (gno.land/adr/pr6381_ledger_entry_after_upgrade.md). The vote carries the
// version: GovDAO sets halt_min_version to the version released
// (RELEASING.md, "Release pages and the upgrade ledger").

func votedHarness(t *testing.T, dockerMode bool) *harness {
	h := newHarnessMode(t, dockerMode, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.cfg.Backup = false
	h.setControl(func(c *control) { c.HaltHeight, c.HaltMinVersion = 30, "v1.6.0" })
	return h
}

func TestVotedVersionWithoutLedgerEntryIsPreparedAndSwitched(t *testing.T) {
	h := votedHarness(t, false)
	h.run()
	h.waitFor("prepared ahead", func() bool { return h.logged("ready for the halt at height 30, version v1.6.0 named by the vote") })
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
	if v := h.currentVersion(); v != "v1.6.0" {
		t.Fatalf("current = %s", v)
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	if st := h.state(); st.LastHalt != 30 || st.Pending != nil {
		t.Fatalf("state = %+v", st)
	}
	dir := h.lay.Upgrade("v1.6.0")
	b, err := os.ReadFile(filepath.Join(layout.Gnoroot(dir), "gnovm", "stdlibs", "VERSION"))
	if err != nil || strings.TrimSpace(string(b)) != "v1.6.0" {
		t.Fatalf("GNOROOT at the wrong tag: %q %v", b, err)
	}
	if i, _ := layout.ReadInfo(dir); i.Commit != h.f.Releases[1].Commit || i.Image != h.f.Releases[1].ImageDigest {
		t.Fatalf("version.json = %+v", i)
	}
}

func TestDockerVotedVersionWithoutLedgerEntryIsPreparedAndSwitched(t *testing.T) {
	h := votedHarness(t, true)
	h.run()
	h.waitFor("prepared ahead", func() bool { return h.logged("ready for the halt at height 30, version v1.6.0 named by the vote") })
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
	dir, _ := h.lay.CurrentDir()
	info, _ := layout.ReadInfo(dir)
	if info.Version != "v1.6.0" || info.ImageRef != "ghcr.io/gnolang/gno/gnoland@"+h.f.Releases[1].ImageDigest {
		t.Fatalf("version.json = %+v", info)
	}
}

func TestVotedVersionReachedWhileGnoVisorWasStopped(t *testing.T) {
	// GnoVisor stopped at the halt, before the switch: the node is stopped
	// and its RPC closed, so the vote is read from what was recorded.
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	if err := h.lay.WriteState(layout.State{Pending: &layout.Pending{Height: 30, MinVersion: "v1.6.0"}}); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(h.height, []byte("30"), 0o644)
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.6.0" {
		t.Fatalf("with a pending halt the old binary must never start: %v", starts)
	}
}

func TestChainSlowToRestartKeepsTheNewVersionRunning(t *testing.T) {
	// D20: after the switch, the chain may stay halted long after
	// restart_timeout (2026-10-29, onyx-1, block 754001). The node is on the
	// right version: it keeps running and catches up when the chain resumes.
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.cfg.RestartTimeout = 300 * time.Millisecond
	h.cfg.Backup = false
	h.setControl(func(c *control) { c.HaltHeight = 30; c.Ranges["v1.6.0"] = [2]int64{1000, 0} })
	h.run()
	h.waitFor("still running", func() bool { return h.logged("the node keeps running") })
	time.Sleep(time.Second)
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" || h.logged("waiting for a version other") {
		t.Fatalf("node runs = %v", starts)
	}
	h.setControl(func(c *control) { c.Ranges["v1.6.0"] = [2]int64{31, 0} })
	h.waitFor("chain resumed", func() bool { return h.chainHeight() >= 33 })
}

func TestLedgerWrittenAfterTheHaltNamingAnotherVersionIsTaken(t *testing.T) {
	// D21: the vote named v1.6.0 and GnoVisor runs it; gno.land then writes
	// the ledger entry for the halt with v1.6.1. GnoVisor switches to it.
	oldL := LedgerInterval
	LedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { LedgerInterval = oldL })

	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30), rel("upgrade", "v1.6.1", 300))
	h.cfg.RestartTimeout = 300 * time.Millisecond
	h.cfg.Backup = false
	h.setControl(func(c *control) {
		c.HaltHeight, c.HaltMinVersion = 30, "v1.6.0"
		c.Ranges["v1.5.0"] = [2]int64{1, 30}
		c.Ranges["v1.6.0"] = [2]int64{1000, 0} // produces no block after the halt
		c.Ranges["v1.6.1"] = [2]int64{31, 0}
	})
	h.run()
	h.waitFor("v1.6.0 from the vote", func() bool { return h.currentVersion() == "v1.6.0" && h.logged("the node keeps running") })
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
	h.waitFor("switch to the ledger's version", func() bool { return h.chainHeight() >= 32 })
	if v := h.currentVersion(); v != "v1.6.1" {
		t.Fatalf("current = %s", v)
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0,v1.6.1" {
		t.Fatalf("node runs = %v", starts)
	}
}

func TestVersionForAHalt(t *testing.T) {
	// The order of D19 and D18: added by hand, then the ledger, then the vote.
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	l, err := ledger.Parse(h.f.Ledger(2))
	if err != nil {
		t.Fatal(err)
	}
	hand := local.File{Upgrades: []local.Entry{{Version: "v1.6.5", HaltHeight: 30}}}
	for _, c := range []struct {
		l         *ledger.Ledger
		f         local.File
		halt      int64
		floor     string
		v, source string
	}{
		{l, hand, 30, "v1.6.0", "v1.6.5", SourceHand},
		{l, local.File{}, 30, "v1.6.2", "v1.6.0", SourceLedger},
		{l, local.File{}, 40, "v1.7.0", "v1.7.0", SourceVote},
		{nil, local.File{}, 40, "v1.7.0", "v1.7.0", SourceVote},
		{l, local.File{}, 40, "", "", ""},
		{l, local.File{}, 40, "not-a-version", "", ""},
	} {
		v, src := VersionForHalt(c.l, c.f, c.halt, c.floor)
		if v != c.v || src != c.source {
			t.Errorf("halt %d floor %q: got %s %q, want %s %q", c.halt, c.floor, v, src, c.v, c.source)
		}
	}
}
