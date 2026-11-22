// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AviaOne/gnovisor/internal/docker"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/prepare"
	"github.com/AviaOne/gnovisor/internal/testutil"
)

// Issue #1 of AviaOne/gnovisor: a node that reaches a halt while it catches
// up exits instead of staying up with its RPC open. These tests run the fake
// node as such a node: at the end of its range it writes the halt panic and
// exits with status 2 before any poll reads the halt height.

func past(v string, halt int64) *testutil.Release {
	r := rel("upgrade", v, halt)
	r.Halted = true
	return r
}

const (
	floorLine = `Error: unable to create the node: binary version "%s" does not meet the minimum version "%s" required by governance; please upgrade to a compatible binary before restarting`
	earlyLine = `Error: unable to create the node: binary version "%s" is an upgrade intended for halt height %d, but the chain is at height %d; please use the previous binary until the halt, or set skip_upgrade_height = %d in config.toml if you have already migrated`
)

func line(format string, args ...any) string { return strings.TrimSpace(fmt.Sprintf(format, args...)) }

func TestSyncingNodeExitingAtPastHaltsIsSwitched(t *testing.T) {
	// The new node synced from genesis, across past halts of the ledger.
	for _, mode := range []string{"binary", "docker"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarnessMode(t, mode == "docker", 3, rel("genesis", "v1.5.0", 0), past("v1.6.0", 20), past("v1.7.0", 40))
			h.setControl(func(c *control) { c.HaltHeight = 40; c.PanicAtHalt = true })
			h.run()
			h.waitFor("synced past both halts", func() bool { return h.chainHeight() >= 45 })
			if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0,v1.7.0" {
				t.Fatalf("node runs = %v", starts)
			}
			if st := h.state(); st.LastHalt != 40 || st.Pending != nil {
				t.Fatalf("state = %+v", st)
			}
			// Decision D23: no copy of the database at a halt the network
			// has long crossed.
			if _, err := os.Stat(h.cfg.BackupDir); !os.IsNotExist(err) {
				t.Fatalf("no backup expected at past halts: %v", err)
			}
		})
	}
}

func TestSyncingNodeExitingAtAVotedHaltWithAVersionAddedByHand(t *testing.T) {
	// Issue #1, first run on the local chain: the version given with
	// add-upgrade -halt, no ledger entry, and the halt height never read over
	// RPC. The node's config.toml names no halt: the halt is the vote's.
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.setControl(func(c *control) {
		c.PanicAtHalt = true
		c.Ranges["v1.5.0"] = [2]int64{1, 20}
		c.Ranges["v1.6.0"] = [2]int64{21, 0}
	})
	h.addLocal("v1.6.0", 20, false)
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 25 })
	if h.currentVersion() != "v1.6.0" || strings.Join(h.nodeStarts(), ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("current = %s, runs %v", h.currentVersion(), h.nodeStarts())
	}
	// A halt the ledger does not record as past: backed up as before.
	if _, err := os.Stat(filepath.Join(h.cfg.BackupDir, "20-v1.5.0", "db", "data")); err != nil {
		t.Fatal("no backup")
	}
}

func TestSyncingNodeExitingAtAVotedHaltOfTheLedger(t *testing.T) {
	// Issue #1, second run: the version known from a ledger row, its floor
	// set by the vote.
	up := rel("upgrade", "v1.6.0", 20)
	up.MinVersion = "v1.6.0"
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), up)
	h.setControl(func(c *control) { c.HaltHeight, c.HaltMinVersion, c.PanicAtHalt = 20, "v1.6.0", true })
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 25 })
	if h.currentVersion() != "v1.6.0" {
		t.Fatalf("current = %s", h.currentVersion())
	}
}

func TestExitAtTheLocalHaltOfConfigTomlIsNotAnUpgrade(t *testing.T) {
	// D15 and D25: a halt_height in the node's own config.toml, which no vote
	// and no ledger entry names, is a manual halt: not an upgrade, even with a
	// version added by hand for that height. The node stays stopped, nothing
	// is started again, and GnoVisor says how to go on.
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.setLocalHalt(12)
	h.setControl(func(c *control) { c.PanicAtHalt = true; c.Ranges["v1.5.0"] = [2]int64{1, 12} })
	h.addLocal("v1.5.9", 12, false)
	h.run()
	h.waitFor("the manual halt reported", func() bool { return h.logged("set halt_height = 0 in config.toml, then restart the service") })
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-h.done:
		t.Fatalf("gnovisor must stay up at a manual halt, it ended: %v", err)
	default:
	}
	if h.currentVersion() != "v1.5.0" || len(h.nodeStarts()) != 1 {
		t.Fatalf("a manual halt must change nothing: current %s, runs %v", h.currentVersion(), h.nodeStarts())
	}
}

func TestManualHaltRightAfterASwitchIsTheSame(t *testing.T) {
	// The same manual halt, met while GnoVisor waits for the first block after
	// a switch: the same answer, not a watch for another version.
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), past("v1.6.0", 20))
	h.cfg.Backup = false
	h.cfg.PollInterval = time.Second // the node reaches 25 before the first block is read
	h.setLocalHalt(25)
	h.setControl(func(c *control) { c.PanicAtHalt = true; c.LocalHalt = 25 })
	h.run()
	h.waitFor("the manual halt reported", func() bool { return h.logged("set halt_height = 0 in config.toml, then restart the service") })
	time.Sleep(500 * time.Millisecond)
	if h.logged("waiting for a version other than") {
		t.Fatal("a manual halt is not a failed version")
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" || h.currentVersion() != "v1.6.0" {
		t.Fatalf("node runs = %v, current %s", starts, h.currentVersion())
	}
	select {
	case err := <-h.done:
		t.Fatalf("gnovisor must stay up at a manual halt, it ended: %v", err)
	default:
	}
}

func TestLocalHaltSetAtTheVotedHeightIsAnUpgrade(t *testing.T) {
	// config.toml suggests halt_height for a coordinated upgrade: set at the
	// height of the halt the ledger records, it is that upgrade, and GnoVisor
	// switches. The node then applies that halt_height at every start, so the
	// new version stops at the same height: GnoVisor says so and waits, with
	// no loop.
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 20))
	h.setLocalHalt(20)
	h.setControl(func(c *control) { c.PanicAtHalt = true; c.LocalHalt = 20 })
	h.run()
	h.waitFor("the config.toml halt reported", func() bool { return h.logged("until halt_height is removed from config.toml") })
	if h.currentVersion() != "v1.6.0" || h.state().LastHalt != 20 {
		t.Fatalf("current = %s, state %+v", h.currentVersion(), h.state())
	}
	time.Sleep(500 * time.Millisecond)
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	// Once halt_height is removed, a restart of GnoVisor takes the node on.
	h.stop()
	h.setLocalHalt(0)
	h.setControl(func(c *control) { c.LocalHalt = 0 })
	h.run()
	h.waitFor("blocks after the halt", func() bool { return h.chainHeight() >= 25 })
}

func TestOldVersionRefusedByTheFloorIsSwitched(t *testing.T) {
	// Issue #1, step 3, as GnoVisor v1.2.0 leaves a node: stopped at the
	// halt, nothing pending, the old version refused by the vote's floor.
	up := past("v1.6.0", 20)
	up.MinVersion = "v1.6.0"
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), up)
	_ = os.WriteFile(h.height, []byte("20"), 0o644)
	h.setControl(func(c *control) {
		c.HaltHeight, c.HaltMinVersion = 20, "v1.6.0"
		c.Refuse = map[string]refusal{"v1.5.0": {From: 20, Line: line(floorLine, "v1.5.0", "v1.6.0")}}
	})
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 25 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	if st := h.state(); st.LastHalt != 20 || st.Pending != nil {
		t.Fatalf("state = %+v", st)
	}
}

func TestOldVersionRefusedWaitsUntilTheHaltIsKnown(t *testing.T) {
	// The floor names the version but not the height, and nothing knows the
	// halt yet: the node stays stopped, never restarted on the old version,
	// until the ledger names the halt.
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	up := past("v1.6.0", 20)
	up.MinVersion = "v1.6.0"
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), up)
	_ = os.WriteFile(h.height, []byte("20"), 0o644)
	h.setControl(func(c *control) {
		c.Refuse = map[string]refusal{"v1.5.0": {From: 20, Line: line(floorLine, "v1.5.0", "v1.6.0")}}
	})
	h.run()
	h.waitFor("wait", func() bool { return h.logged("is not known yet") })
	time.Sleep(500 * time.Millisecond)
	if n := len(h.nodeStarts()); n != 1 {
		t.Fatalf("the old version was started again: %v", h.nodeStarts())
	}
	h.publish(2)
	h.waitFor("switch once published", func() bool { return h.chainHeight() >= 25 })
	if h.currentVersion() != "v1.6.0" {
		t.Fatalf("current = %s", h.currentVersion())
	}
}

func TestNodeRestoredFromAnOlderSnapshotRunsTheVersionOfItsHeight(t *testing.T) {
	// Issue #1: a node restored from an older snapshot, while the version in
	// service is the newer one. The node refuses it below the halt; GnoVisor
	// runs the version the ledger gives for the next block, then crosses the
	// halt.
	up := past("v1.6.0", 20)
	up.MinVersion = "v1.6.0"
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), up)
	h.useVersion(1)
	_ = os.WriteFile(h.height, []byte("10"), 0o644)
	h.setControl(func(c *control) {
		c.HaltHeight, c.HaltMinVersion, c.PanicAtHalt = 20, "v1.6.0", true
		c.Refuse = map[string]refusal{"v1.6.0": {From: 0, To: 19, Line: line(earlyLine, "v1.6.0", 20, 10, 20)}}
	})
	h.run()
	h.waitFor("caught up past the halt", func() bool { return h.chainHeight() >= 25 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.6.0,v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	if h.currentVersion() != "v1.6.0" || h.state().LastHalt != 20 {
		t.Fatalf("current = %s, state %+v", h.currentVersion(), h.state())
	}
}

func TestNodeSyncedOnANewerVersionKeepsItAtPastHalts(t *testing.T) {
	// D26: gno.land syncs a node from genesis on a recent version and restarts
	// it on that same version at every past halt whose floor it meets
	// (misc/deployments/mainnet.gno.land/UPGRADES.md, "Joining from genesis").
	// GnoVisor keeps it, never goes down to the older version the ledger gives,
	// and switches only where a floor asks for a newer version.
	for _, mode := range []string{"binary", "docker"} {
		t.Run(mode, func(t *testing.T) {
			up7 := past("v1.7.0", 40)
			up7.MinVersion = "v1.7.0"
			up8 := past("v1.8.0", 60)
			up8.MinVersion = "v1.8.0"
			h := newHarnessMode(t, mode == "docker", 4, rel("genesis", "v1.5.0", 0), past("v1.6.0", 20), up7, up8)
			h.useVersion(2) // v1.7.0 from genesis
			h.setControl(func(c *control) {
				c.Halts = []int64{20, 40, 60}
				c.Ranges["v1.7.0"] = [2]int64{1, 60}
				c.HaltHeight, c.HaltMinVersion = 60, "v1.8.0"
			})
			h.run()
			h.waitFor("past the three halts", func() bool { return h.chainHeight() >= 65 })
			if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.7.0,v1.7.0,v1.7.0,v1.8.0" {
				t.Fatalf("node runs = %v", starts)
			}
			if st := h.state(); st.LastHalt != 60 || st.Pending != nil || h.currentVersion() != "v1.8.0" {
				t.Fatalf("current %s, state %+v", h.currentVersion(), st)
			}
		})
	}
}

func TestTheSameRefusalTwiceIsNotLooped(t *testing.T) {
	// A version that is refused again the same way is not started again and
	// again: GnoVisor waits for another one.
	up := past("v1.6.0", 20)
	up.MinVersion = "v1.6.0"
	h := newHarness(t, 2, rel("genesis", "v1.5.0", 0), up)
	_ = os.WriteFile(h.height, []byte("20"), 0o644)
	h.setControl(func(c *control) {
		c.Refuse = map[string]refusal{
			"v1.5.0": {From: 20, Line: line(floorLine, "v1.5.0", "v1.6.0")},
			"v1.6.0": {From: 20, Line: line(floorLine, "v1.6.0", "v1.6.0")},
		}
	})
	h.run()
	h.waitFor("watch for another version", func() bool { return h.logged("waiting for a version other than v1.6.0") })
	n := len(h.nodeStarts())
	time.Sleep(time.Second)
	if m := len(h.nodeStarts()); m != n || m > 3 {
		t.Fatalf("the refused version is started in a loop: %v", h.nodeStarts())
	}
}

func TestGnoVisorRestartedAfterTheExitNeverRunsTheOldVersion(t *testing.T) {
	// Issue #1: GnoVisor restarted after the node exited at the halt. The
	// version is not known yet at the exit; the halt stays pending across the
	// restart, and the old version is never started again.
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0), past("v1.6.0", 20))
	h.setControl(func(c *control) { c.PanicAtHalt = true; c.Ranges["v1.6.0"] = [2]int64{21, 0} })
	h.run()
	h.waitFor("pending halt", func() bool { s := h.state(); return s.Pending != nil && s.Pending.Height == 20 })
	h.stop()
	h.publish(2)
	h.run()
	h.waitFor("switch after the restart", func() bool { return h.chainHeight() >= 25 })
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
}

func TestExitWatchReadsTheLinesOfGnoland(t *testing.T) {
	// The texts of gnolang/gno v1.2.0 to v1.8.0, written in odd chunks.
	w := &exitWatch{}
	out := "I[2026-11-20] some log\npanic: halt height 445000 reached, node shutting down\n\ngoroutine 61 [running]:\n" +
		line(floorLine, "v1.7.1", "v1.8.0") + "\n" + line(earlyLine, "v1.8.0", 1651500, 1600000, 1651500)
	for i := 0; i < len(out); i += 7 {
		_, _ = w.Write([]byte(out[i:min(i+7, len(out))]))
	}
	w.flush()
	want := exitFacts{HaltPanic: 445000, Floor: "v1.8.0", EarlyHalt: 1651500, EarlyHeight: 1600000}
	if got := w.get(); got != want {
		t.Fatalf("facts = %+v, want %+v", got, want)
	}
	// A log line that quotes the panic is not a panic.
	w = &exitWatch{}
	_, _ = w.Write([]byte("E[2026-11-20] CONSENSUS FAILURE!!! err=\"panic: halt height 5 reached, node shutting down\"\n"))
	if w.get().HaltPanic != 0 {
		t.Fatal("a quoted panic must not count")
	}
}

func TestLocalHaltHeightReadsTheTopLevelKeyOnly(t *testing.T) {
	home := t.TempDir()
	if _, err := LocalHaltHeight(home); err == nil {
		t.Fatal("a missing config.toml must be an error")
	}
	_ = os.MkdirAll(filepath.Join(home, "config"), 0o755)
	for cfg, want := range map[string]int64{
		"moniker = \"a\"\n[rpc]\nhalt_height = 9\n":            0,
		"halt_height = 1183000 # coordinated upgrade\n[p2p]\n": 1183000,
		"  halt_height=0\n": 0,
	} {
		_ = os.WriteFile(filepath.Join(home, "config", "config.toml"), []byte(cfg), 0o644)
		if got, err := LocalHaltHeight(home); err != nil || got != want {
			t.Fatalf("%q: got %d %v, want %d", cfg, got, err, want)
		}
	}
}

// useVersion makes release i the version in service, prepared from the
// ledger, as a node restored with a newer GnoVisor directory.
func (h *harness) useVersion(i int) {
	h.t.Helper()
	l := h.f.Releases[i]
	full, err := ledger.Parse(h.f.Ledger(-1))
	if err != nil {
		h.t.Fatal(err)
	}
	e, _ := full.Entry(l.Version)
	o := prepare.Options{Platform: "linux/amd64", AutoDownload: true, GnorootRepo: h.f.Repo, HTTPClient: h.f.Client(), Registry: h.f.Server.URL}
	if h.cfg.Docker() {
		o.Docker = &docker.Client{Bin: h.cfg.DockerBin}
	}
	d, err := prepare.Version(context.Background(), h.lay, e, o)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.lay.Switch(d); err != nil {
		h.t.Fatal(err)
	}
}
