// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/prepare"
)

// A node that catches up (synced from genesis, or restored from an older
// snapshot) does not stay up at a halt as a live node does: it exits, and
// GnoVisor may never have read the halt height over RPC. nodeExited reads
// why the node exited, from what it wrote on stderr (node.go), and takes it
// across, as for a live node:
//   - it exited at a halt: the halt is handled (handleHalt), once it is known
//     to be voted; a halt of the node's own config.toml is a manual halt, not
//     an upgrade (decision D15), and the node stays stopped (decision D25);
//   - a vote's floor refused its version: the node is at a halt it could not
//     cross, such as a node left there by GnoVisor v1.2.0; the halt is found
//     by its floor and handled;
//   - its version is meant for a later halt: the node is behind, such as one
//     restored from an older snapshot; it is switched to the version the
//     ledger gives for its next block.
//
// It reports whether it handled the exit. An exit it does not recognise is
// left to the caller, which ends the run as before: systemd restarts
// GnoVisor, and a loop of crashes stays visible.

// nodeExited handles an exit of the node GnoVisor did not ask for.
func (s *Supervisor) nodeExited(ctx context.Context) (bool, error) {
	f := s.node.watch.get()
	desc := s.node.exitDescription()
	if f == (exitFacts{}) {
		s.logf("node exited (%s) with no halt and no version refusal in its output", desc)
		return false, nil
	}
	// The same exit again on the same version: acting again would loop.
	key := f.String() + "/" + s.current.Version
	if key == s.lastExit {
		s.logf("node exited again the same way on %s (%s): not starting it again", s.current.Version, desc)
		if h := f.HaltPanic; h > 0 {
			return true, s.awaitFix(ctx, h)
		}
		if h, ok := s.haltOfFloor(f.Floor); ok {
			return true, s.awaitFix(ctx, h)
		}
		return true, s.blocked(ctx)
	}
	s.lastExit = key
	s.refreshLedger(ctx, true)

	switch {
	case f.HaltPanic > 0:
		h := f.HaltPanic
		if h == s.state.LastHalt {
			// The switch for this halt is done, and the new version stops at
			// the same height: the halt_height of config.toml, applied at
			// every start (gno.land/cmd/gnoland/start.go, SetHaltHeight), keeps
			// any version from going past it.
			if local, err := LocalHaltHeight(s.Layout.Home); err == nil && local == h {
				s.logf("node exited at height %d (%s), the halt_height of its config.toml: the switch for this halt is done, and no version goes past that height until halt_height is removed from config.toml", h, desc)
				return true, s.blocked(ctx)
			}
		}
		if !s.votedHalt(h) {
			// A manual halt the operator set on purpose (an attack, a chain
			// relaunch): it is respected, the node is not started again, and
			// lifting it is the operator's act as well (decision D25). The same
			// at any moment, in service or right after a switch.
			s.logf("node exited at height %d (%s), the halt_height of its config.toml, which no vote and no ledger entry names: not an upgrade; the node stays stopped. To go on: set halt_height = 0 in config.toml, then restart the service", h, desc)
			return true, s.blocked(ctx)
		}
		s.logf("node exited at the halt at height %d (%s): it was catching up; handling the halt", h, desc)
		return true, s.handleHalt(ctx, h, s.floorFor(h))

	case f.Floor != "":
		s.logf("node refused to start (%s): %s is below %s, the version a vote requires", desc, s.current.Version, f.Floor)
		for {
			if h, ok := s.haltOfFloor(f.Floor); ok {
				s.logf("the halt whose floor is %s is at height %d: handling it", f.Floor, h)
				return true, s.handleHalt(ctx, h, f.Floor)
			}
			s.logf("the height of the halt whose floor is %s is not known yet: the node stays stopped; the ledger is read every %s, or give the version with gnovisor add-upgrade -halt", f.Floor, PendingLedgerInterval)
			if err := s.sleep(ctx, PendingLedgerInterval); err != nil {
				return true, err
			}
			s.refreshLedger(ctx, true)
		}

	case f.EarlyHalt > 0:
		x := f.EarlyHeight
		s.logf("node refused to start (%s): %s is meant for the halt at height %d and the node is at height %d", desc, s.current.Version, f.EarlyHalt, x)
		return true, s.catchUpFrom(ctx, x)
	}
	return false, nil
}

// votedHalt reports whether a halt the node exited at was voted. Only two
// things set the height a node halts at (gno.land, SetHaltHeight): its
// config.toml (gno.land/cmd/gnoland/start.go) and a GovDAO vote
// (gno.land/pkg/gnoland/app.go, EndBlocker). A height the vote or the ledger
// names is a vote, even when config.toml names it too, as its comment
// suggests for a coordinated upgrade; any other height than config.toml's is
// a vote as well.
func (s *Supervisor) votedHalt(h int64) bool {
	if h == s.chainHalt.Height {
		return true
	}
	if s.ledger != nil {
		if _, ok := s.ledger.UpgradeAt(h); ok {
			return true
		}
	}
	local, err := LocalHaltHeight(s.Layout.Home)
	if err != nil {
		s.logf("reading the halt_height of the node's config.toml: %v", err)
		return false
	}
	return local != h
}

// floorFor returns the floor the vote set for the halt at h, when known.
func (s *Supervisor) floorFor(h int64) string {
	if h == s.chainHalt.Height {
		return s.chainHalt.MinVersion
	}
	if s.ledger != nil {
		if e, ok := s.ledger.UpgradeAt(h); ok && e.HaltMinVersion != nil {
			return *e.HaltMinVersion
		}
	}
	return ""
}

// haltOfFloor finds the height of the halt whose vote set floor: the halt
// last read on the chain, the ledger, or a version added by hand for a halt.
func (s *Supervisor) haltOfFloor(floor string) (int64, bool) {
	if floor == "" {
		return 0, false
	}
	if s.chainHalt.Height > 0 && s.chainHalt.MinVersion == floor {
		return s.chainHalt.Height, true
	}
	if s.ledger != nil {
		for _, e := range s.ledger.Upgrades {
			if e.Kind == ledger.KindUpgrade && e.HaltHeight != nil && e.HaltMinVersion != nil && *e.HaltMinVersion == floor {
				return *e.HaltHeight, true
			}
		}
		for _, e := range s.ledger.Upgrades {
			if e.Kind == ledger.KindUpgrade && e.HaltHeight != nil && e.Version == floor {
				return *e.HaltHeight, true
			}
		}
	}
	if f, err := local.Read(s.Layout); err == nil {
		for _, e := range f.Upgrades {
			if e.HaltHeight > 0 && e.Version == floor {
				return e.HaltHeight, true
			}
		}
	}
	return 0, false
}

// catchUpFrom runs, for a node at height x, the version that serves block
// x+1: the one added by hand for the halt that opened that range (D19), or
// the newest of the range in the ledger.
func (s *Supervisor) catchUpFrom(ctx context.Context, x int64) error {
	for {
		if s.ledger != nil {
			for _, r := range s.ledger.BlockRanges() {
				if x+1 < r.From || (r.To != 0 && x+1 > r.To) {
					continue
				}
				h := r.From - 1 // the halt that opened the range, 0 for genesis
				if loc, ok := s.localAt(h); ok && h > 0 {
					if dir, err := s.localDir(loc); err == nil {
						info, _ := layout.ReadInfo(dir)
						return s.switchFor(ctx, h, dir, info, x+1)
					}
				}
				v := r.Newest()
				if v == s.current.Version {
					s.logf("the ledger gives %s, the version in service, for block %d: nothing else to run", v, x+1)
					return s.blocked(ctx)
				}
				e, _ := s.ledger.Entry(v)
				dir, err := prepare.Version(ctx, s.Layout, e, s.prepOpts())
				if err == nil {
					var info layout.Info
					if info, err = layout.Verify(dir); err == nil {
						s.logf("running %s, the version the ledger gives for block %d", v, x+1)
						return s.switchFor(ctx, h, dir, info, x+1)
					}
				}
				s.logf("version %s for block %d is not ready: %v", v, x+1, err)
			}
		}
		s.logf("no version ready for block %d: the node stays stopped; the ledger is read every %s", x+1, PendingLedgerInterval)
		if err := s.sleep(ctx, PendingLedgerInterval); err != nil {
			return err
		}
		s.refreshLedger(ctx, true)
	}
}

// switchFor switches to the version that serves the blocks after the halt
// at h (0: from genesis) for a node behind the chain, and waits for block
// wantHeight.
func (s *Supervisor) switchFor(ctx context.Context, h int64, dir string, info layout.Info, wantHeight int64) error {
	if h > 0 {
		return s.switchTo(ctx, h, dir, info, wantHeight)
	}
	from := s.current.Version
	s.stopNode()
	if dir != s.currentDir {
		if err := s.Layout.Switch(dir); err != nil {
			s.logf("switch failed, node left stopped: %v", err)
			return s.blocked(ctx)
		}
		s.logf("switched from %s to %s", from, info.Version)
	}
	s.current, s.currentDir = info, dir
	s.state.LastHalt, s.state.Pending = 0, nil
	if err := s.Layout.WriteState(s.state); err != nil {
		return err
	}
	return s.restartAndWait(ctx, wantHeight, 0)
}

// LocalHaltHeight returns the halt_height of the node's own config.toml,
// <node dir>/config/config.toml (gno.land/cmd/gnoland/start.go,
// constructConfigPath), 0 when not set. The key is at the top level of the
// file (tm2/pkg/bft/config/config.go, BaseConfig squashed into Config), so
// the first table header ends the search.
func LocalHaltHeight(home string) (int64, error) {
	path := filepath.Join(home, "config", "config.toml")
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "halt_height" {
			continue
		}
		val = strings.TrimSpace(val)
		if i := strings.IndexByte(val, '#'); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: halt_height %q: %w", path, val, err)
		}
		return n, nil
	}
	return 0, sc.Err()
}
