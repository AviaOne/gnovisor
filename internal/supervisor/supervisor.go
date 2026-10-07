// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package supervisor runs gnoland and takes it through coordinated halts,
// past halts met while syncing, and rolling releases.
//
// Facts it relies on, from gnolang/gno at commit 156777e0
// (gno.land/adr/pr5368_govdao_halt_height.md):
//   - at a halt the last committed block is halt_height, consensus stops,
//     and the gnoland process stays alive with its RPC open: a halt is seen
//     as a node that stays at halt_height without a new block;
//   - with no version floor nothing stops the old binary from resuming the
//     chain after the halt, so GnoVisor never restarts the old binary after
//     a halt (decision D4);
//   - a halt_height can also come from the node's own config.toml; such a
//     local halt matches no chain parameter and no ledger entry, and is not
//     an upgrade.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/AviaOne/gnovisor/internal/backup"
	"github.com/AviaOne/gnovisor/internal/config"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/prepare"
	"github.com/AviaOne/gnovisor/internal/rpc"
	"github.com/AviaOne/gnovisor/internal/semver"
	"github.com/AviaOne/gnovisor/internal/version"
)

// LedgerInterval is how often the ledger is read again (specification,
// section 10).
var LedgerInterval = 10 * time.Minute

// PendingLedgerInterval is how often the ledger is read while a halt has
// been reached and its version is still missing from the ledger: every
// minute of that wait is a minute the node is stopped (decision Q4).
var PendingLedgerInterval = time.Minute

// Supervisor supervises one node.
type Supervisor struct {
	Cfg      *config.Config
	Layout   layout.Layout
	Platform string
	Log      func(format string, args ...any)
	// HTTPClient downloads the ledger and the binaries; nil for defaults.
	HTTPClient *http.Client
	// Registry is the image registry; empty for ghcr.io.
	Registry string

	rpc         *rpc.Client
	ledger      *ledger.Ledger
	ledgerAt    time.Time
	node        *node
	current     layout.Info
	currentDir  string
	state       layout.State
	lastHeight  int64
	lastChange  time.Time
	prepared    map[int64]bool
	rollingDir  map[string]string
	warnedLocal int64
}

func (s *Supervisor) logf(format string, args ...any) { s.Log(format, args...) }

func (s *Supervisor) prepOpts() prepare.Options {
	return prepare.Options{
		Platform:     s.Platform,
		AutoDownload: s.Cfg.AutoDownload,
		GnorootRepo:  s.Cfg.GnorootRepo,
		HTTPClient:   s.HTTPClient,
		Registry:     s.Registry,
		Log:          s.Log,
	}
}

// Run supervises until ctx is cancelled (SIGTERM or SIGINT) or the node
// exits by itself. It returns nil on a requested stop.
func (s *Supervisor) Run(ctx context.Context) error {
	s.rpc = rpc.New(s.Cfg.RPC)
	s.prepared = map[int64]bool{}
	s.rollingDir = map[string]string{}
	var err error
	if s.state, err = s.Layout.ReadState(); err != nil {
		return err
	}
	if err := s.loadCurrent(); err != nil {
		return err
	}
	s.logf("starting, %s, node version %s, chain %s", version.Attribution, s.current.Version, s.Cfg.ChainID)
	s.refreshLedger(ctx, true)

	if s.state.Pending != nil {
		// The node halted at Pending.Height and its version was not ready.
		// Starting it on the old binary could resume the chain (D4).
		s.logf("halt at height %d is pending, the node stays stopped until its version is ready", s.state.Pending.Height)
		if err := s.handleHalt(ctx, s.state.Pending.Height); err != nil {
			if errors.Is(err, context.Canceled) {
				s.stopNode()
				return nil
			}
			return err
		}
	} else if err := s.start(); err != nil {
		return err
	}
	if err := s.checkChainID(ctx); err != nil {
		s.stopNode()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	tick := time.NewTicker(s.Cfg.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logf("stop requested")
			s.stopNode()
			return nil
		case <-s.nodeDone():
			if !s.node.stopped {
				// Not restarted here: systemd restarts GnoVisor, and a loop
				// of crashes stays visible (specification, 7.6).
				return fmt.Errorf("node exited by itself: %s", s.node.exitDescription())
			}
		case <-tick.C:
			if err := s.step(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					s.stopNode()
					return nil
				}
				return err
			}
		}
	}
}

func (s *Supervisor) nodeDone() <-chan struct{} {
	if s.node == nil {
		return nil
	}
	return s.node.done
}

func (s *Supervisor) loadCurrent() error {
	dir, err := s.Layout.CurrentDir()
	if err != nil {
		return fmt.Errorf("no current version: %w (run gnovisor init)", err)
	}
	info, err := layout.Verify(dir)
	if err != nil {
		return fmt.Errorf("current version refused: %w", err)
	}
	s.current, s.currentDir = info, dir
	return nil
}

func (s *Supervisor) start() error {
	n, err := startNode(s.currentDir, s.Layout.Home, s.Cfg.NodeArgs)
	if err != nil {
		return fmt.Errorf("start %s: %w", s.current.Version, err)
	}
	s.node = n
	s.lastHeight, s.lastChange = 0, time.Now()
	s.logf("node started, version %s, pid %d", s.current.Version, n.cmd.Process.Pid)
	return nil
}

func (s *Supervisor) stopNode() {
	if s.node == nil {
		return
	}
	if err := s.node.stop(s.Cfg.ShutdownGrace); err != nil {
		s.logf("%v", err)
	}
	s.logf("node stopped")
	s.node = nil
}

// checkChainID waits for the RPC and compares the chain with the config.
func (s *Supervisor) checkChainID(ctx context.Context) error {
	deadline := time.Now().Add(s.Cfg.RestartTimeout)
	for {
		st, err := s.rpc.Status(ctx)
		if err == nil {
			if st.Network != s.Cfg.ChainID {
				return fmt.Errorf("the node serves chain %q, gnovisor.toml says %q", st.Network, s.Cfg.ChainID)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("RPC %s did not answer within %s: %w", s.Cfg.RPC, s.Cfg.RestartTimeout, err)
		}
		if err := s.sleep(ctx, s.Cfg.PollInterval); err != nil {
			return err
		}
		if s.node != nil {
			select {
			case <-s.node.done:
				return fmt.Errorf("node exited by itself: %s", s.node.exitDescription())
			default:
			}
		}
	}
}

func (s *Supervisor) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// refreshLedger reads the ledger when it is due. A ledger that fails to
// load or validate is ignored and the last valid one kept (section 10).
func (s *Supervisor) refreshLedger(ctx context.Context, force bool) {
	if !force && s.ledger != nil && time.Since(s.ledgerAt) < LedgerInterval {
		return
	}
	if !force && s.ledger == nil && time.Since(s.ledgerAt) < s.Cfg.PollInterval*12 {
		return
	}
	s.ledgerAt = time.Now()
	data, err := s.fetchLedger(ctx)
	if err == nil {
		var l *ledger.Ledger
		if l, err = ledger.Parse(data); err == nil {
			if err = l.Validate(); err == nil {
				if l.ChainID != s.Cfg.ChainID {
					err = fmt.Errorf("ledger is for chain %q, gnovisor.toml says %q", l.ChainID, s.Cfg.ChainID)
				} else {
					s.ledger = l
					return
				}
			}
		}
	}
	s.logf("ledger %s refused, keeping the last valid one: %v", s.Cfg.LedgerURL, err)
}

func (s *Supervisor) fetchLedger(ctx context.Context) ([]byte, error) {
	u := s.Cfg.LedgerURL
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return os.ReadFile(u)
	}
	c := s.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// step is one poll of the node.
func (s *Supervisor) step(ctx context.Context) error {
	s.refreshLedger(ctx, false)
	st, err := s.rpc.Status(ctx)
	if err != nil {
		return nil // the node may be starting; its exit is caught by Run
	}
	if st.Height != s.lastHeight {
		s.lastHeight, s.lastChange = st.Height, time.Now()
	}
	halt, err := s.rpc.Halt(ctx)
	if err != nil {
		s.logf("reading the halt parameters: %v", err)
		return nil
	}

	scheduled := halt.Height > st.Height
	if scheduled && !s.prepared[halt.Height] && s.ledger != nil {
		if e, ok := s.ledger.UpgradeAt(halt.Height); ok {
			if _, err := prepare.Version(ctx, s.Layout, e, s.prepOpts()); err != nil {
				s.logf("preparing %s for the halt at height %d: %v", e.Version, halt.Height, err)
			} else {
				s.prepared[halt.Height] = true
				s.logf("ready for the halt at height %d, version %s", halt.Height, e.Version)
			}
		} else if e, ok := s.localAt(halt.Height); ok {
			if _, err := s.localDir(e); err != nil {
				s.logf("version %s added by hand for the halt at height %d: %v", e.Version, halt.Height, err)
			} else {
				s.prepared[halt.Height] = true
				s.logf("ready for the halt at height %d, version %s added by hand", halt.Height, e.Version)
			}
		} else if time.Since(s.ledgerAt) < s.Cfg.PollInterval {
			// Once per ledger read (section 7.2).
			s.logf("halt programmed at height %d, no ledger entry for it yet", halt.Height)
		}
	}

	if h := st.Height; h > 0 && h != s.state.LastHalt {
		// At the height of a halt the chain or the ledger names, block h is
		// committed and the node produces nothing more (ADR pr5368): there
		// is nothing to confirm, and waiting would only lengthen the outage
		// (G12). halt_confirm is kept for a node that stops elsewhere.
		_, inLedger := s.ledgerUpgradeAt(h)
		_, inLocal := s.localAt(h)
		if halt.Height == h || inLedger || inLocal {
			s.logf("halt reached at height %d", h)
			return s.handleHalt(ctx, h)
		}
		if time.Since(s.lastChange) >= s.Cfg.HaltConfirm {
			if s.warnedLocal != h {
				s.warnedLocal = h
				s.logf("node stays at height %d, which is neither the chain halt nor a ledger halt: not an upgrade, nothing done", h)
			}
			return nil
		}
	}

	if !scheduled && !st.CatchingUp {
		return s.maybeRolling(ctx, st.Height)
	}
	return nil
}

func (s *Supervisor) ledgerUpgradeAt(h int64) (*ledger.Entry, bool) {
	if s.ledger == nil {
		return nil, false
	}
	return s.ledger.UpgradeAt(h)
}

// target returns the version to run after the halt at h: the newest of
// the range that starts at h+1, rolling patches included.
func (s *Supervisor) target(h int64) (ledger.Range, bool) {
	if s.ledger == nil {
		return ledger.Range{}, false
	}
	for _, r := range s.ledger.BlockRanges() {
		if r.From == h+1 {
			return r, true
		}
	}
	return ledger.Range{}, false
}

// handleHalt takes the node across the halt at h (sections 7.3 and 7.4).
// The node may be running (halted) or already stopped (pending halt).
func (s *Supervisor) handleHalt(ctx context.Context, h int64) error {
	// The ledger is read again now: the one in memory can be up to
	// LedgerInterval old, and gno.land may have named another version for
	// this halt in the meantime (a last-minute fix is a new tag).
	s.refreshLedger(ctx, true)
	for {
		r, ok := s.target(h)
		loc, hasLoc := s.localAt(h)
		if ok && hasLoc {
			if err := s.sameAsOfficial(loc, r.Newest()); err != nil {
				// Two sources disagree on what runs after this halt, and
				// nothing tells which one is right: no switch (D9).
				s.logf("halt at height %d: %v; no switch, the node stays stopped", h, err)
				if err := s.waitPending(ctx, h); err != nil {
					return err
				}
				continue
			}
		}
		if !ok && hasLoc {
			dir, err := s.localDir(loc)
			if err == nil {
				info, _ := layout.ReadInfo(dir)
				s.logf("halt at height %d: using version %s added by hand", h, loc.Version)
				return s.switchTo(ctx, h, dir, info, h+1)
			}
			s.logf("version %s added by hand for the halt at height %d: %v", loc.Version, h, err)
			if err := s.waitPending(ctx, h); err != nil {
				return err
			}
			continue
		}
		if ok && r.Serves(s.current.Version) {
			// Already on a version that serves the next blocks: a restart
			// is enough (section 7.4).
			s.logf("version %s serves the blocks after %d, restarting the node", s.current.Version, h)
			return s.switchTo(ctx, h, s.currentDir, s.current, h+1)
		}
		if ok {
			e, _ := s.ledger.Entry(r.Newest())
			dir, err := prepare.Version(ctx, s.Layout, e, s.prepOpts())
			if err == nil {
				info, err := layout.Verify(dir)
				if err == nil {
					return s.switchTo(ctx, h, dir, info, h+1)
				}
			}
			s.logf("version %s for the halt at height %d is not ready: %v", e.Version, h, err)
		} else {
			s.logf("no ledger entry for the halt at height %d yet: the node stays stopped, the old binary is never restarted", h)
		}
		if err := s.waitPending(ctx, h); err != nil {
			return err
		}
	}
}

// waitPending records the halt as pending, keeps the node stopped, waits
// PendingLedgerInterval and reads the ledger again. The file of versions
// added by hand is read at every pass, so `gnovisor add-upgrade` run during
// the wait is taken within a minute.
func (s *Supervisor) waitPending(ctx context.Context, h int64) error {
	if err := s.setPending(h); err != nil {
		return err
	}
	s.stopNode()
	if err := s.sleep(ctx, PendingLedgerInterval); err != nil {
		return err
	}
	s.refreshLedger(ctx, true)
	return nil
}

// localAt returns the version added by hand for the halt at h.
func (s *Supervisor) localAt(h int64) (local.Entry, bool) {
	f, err := local.Read(s.Layout)
	if err != nil {
		s.logf("%v", err)
		return local.Entry{}, false
	}
	return f.AtHalt(h)
}

// localDir checks the directory of a version added by hand against the
// sha256 recorded by add-upgrade.
func (s *Supervisor) localDir(e local.Entry) (string, error) {
	dir := s.Layout.VersionDir(e.Version)
	if dir == "" {
		return "", fmt.Errorf("%s is not in %s", e.Version, s.Layout.Upgrades())
	}
	info, err := layout.Verify(dir)
	if err != nil {
		return "", err
	}
	if info.SHA256 != e.SHA256 {
		return "", fmt.Errorf("%s has sha256 %s, add-upgrade recorded %s", dir, info.SHA256, e.SHA256)
	}
	return dir, nil
}

// sameAsOfficial reports whether a version added by hand is the one the
// ledger gives for the same halt, binary included.
func (s *Supervisor) sameAsOfficial(e local.Entry, official string) error {
	oe, ok := s.ledger.Entry(official)
	if !ok {
		return fmt.Errorf("ledger entry %s missing", official)
	}
	_, sha, err := oe.Binary(s.Platform)
	if err != nil {
		return err
	}
	if e.Version != official || e.SHA256 != sha {
		return fmt.Errorf("the ledger gives %s (sha256 %s), the version added by hand is %s (sha256 %s)", official, sha, e.Version, e.SHA256)
	}
	return nil
}

func (s *Supervisor) setPending(h int64) error {
	if s.state.Pending != nil && s.state.Pending.Height == h {
		return nil
	}
	s.state.Pending = &layout.Pending{Height: h}
	return s.Layout.WriteState(s.state)
}

// switchTo stops the node, backs it up, points current at dir, records the
// halt, restarts and waits for block wantHeight. On failure the node is
// left stopped and GnoVisor never goes back to the old version: the backup
// and the old version directory stay for a manual intervention.
// A switch, once started, is not interrupted by a stop request.
func (s *Supervisor) switchTo(ctx context.Context, h int64, dir string, info layout.Info, wantHeight int64) error {
	if err := s.setPending(h); err != nil {
		return err
	}
	from := s.current.Version
	s.stopNode()
	if s.Cfg.Backup {
		dst, err := backup.Make(s.Layout.Home, s.Cfg.BackupDir, backup.Name(h, from))
		if err != nil {
			s.logf("backup failed, no switch, node left stopped: %v", err)
			return s.blocked(ctx)
		}
		s.logf("backup done in %s", dst)
	}
	if dir != s.currentDir {
		if err := s.Layout.Switch(dir); err != nil {
			s.logf("switch failed, node left stopped: %v", err)
			return s.blocked(ctx)
		}
		s.logf("switched from %s to %s", from, info.Version)
	}
	s.current, s.currentDir = info, dir
	s.state.LastHalt, s.state.Pending = h, nil
	if err := s.Layout.WriteState(s.state); err != nil {
		return err
	}
	return s.restartAndWait(ctx, wantHeight, h)
}

// restartAndWait starts the node and waits for wantHeight within
// restart_timeout. halt is the halt being crossed, 0 for a rolling release:
// after a halt, a version that fails is followed by a watch for its fix.
func (s *Supervisor) restartAndWait(ctx context.Context, wantHeight, halt int64) error {
	failed := func() error {
		if halt > 0 {
			return s.awaitFix(ctx, halt)
		}
		return s.blocked(ctx)
	}
	if err := s.start(); err != nil {
		s.logf("%v; node left stopped", err)
		return failed()
	}
	deadline := time.Now().Add(s.Cfg.RestartTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-s.node.done:
			s.logf("node exited after the switch (%s), left stopped, no return to the old version", s.node.exitDescription())
			s.node = nil
			return failed()
		default:
		}
		if st, err := s.rpc.Status(context.Background()); err == nil && st.Height >= wantHeight {
			s.lastHeight, s.lastChange = st.Height, time.Now()
			s.logf("block %d seen, update succeeded", st.Height)
			return nil
		}
		// A stop request during the wait is honoured: the switch itself is
		// already recorded.
		if err := s.sleep(ctx, s.Cfg.PollInterval); err != nil {
			return err
		}
	}
	s.logf("no block %d within %s, node stopped, no return to the old version", wantHeight, s.Cfg.RestartTimeout)
	s.stopNode()
	return failed()
}

// awaitFix follows a version that did not run after the halt at h: it
// refused to start, exited, or produced no block (an app hash mismatch, for
// example). gno.land then publishes a fix as a new tag and names it in the
// ledger for the blocks after h. The node stays stopped, never on the old
// version, and the ledger and the versions added by hand are read again
// every PendingLedgerInterval: as soon as another version is named for
// those blocks, it is prepared and the switch is made again. The halt is
// recorded as pending, so a restart of GnoVisor resumes this watch.
func (s *Supervisor) awaitFix(ctx context.Context, h int64) error {
	failed := s.current.Version
	if err := s.setPending(h); err != nil {
		return err
	}
	s.stopNode()
	s.logf("waiting for a version other than %s for the blocks after %d; the ledger is read every %s", failed, h, PendingLedgerInterval)
	for {
		if err := s.sleep(ctx, PendingLedgerInterval); err != nil {
			return err
		}
		s.refreshLedger(ctx, true)
		if loc, ok := s.localAt(h); ok && loc.Version != failed {
			if dir, err := s.localDir(loc); err == nil {
				info, _ := layout.ReadInfo(dir)
				s.logf("version %s added by hand for the blocks after %d", loc.Version, h)
				return s.switchTo(ctx, h, dir, info, h+1)
			}
		}
		r, ok := s.target(h)
		if !ok || r.Newest() == failed {
			continue
		}
		e, _ := s.ledger.Entry(r.Newest())
		dir, err := prepare.Version(ctx, s.Layout, e, s.prepOpts())
		if err != nil {
			s.logf("version %s named for the blocks after %d is not ready: %v", e.Version, h, err)
			continue
		}
		info, err := layout.Verify(dir)
		if err != nil {
			s.logf("version %s: %v", e.Version, err)
			continue
		}
		s.logf("the ledger now names %s for the blocks after %d", e.Version, h)
		return s.switchTo(ctx, h, dir, info, h+1)
	}
}

// blocked keeps GnoVisor alive with the node stopped until a stop request,
// so that systemd does not restart the failed sequence in a loop.
func (s *Supervisor) blocked(ctx context.Context) error {
	s.stopNode()
	s.logf("waiting for an operator; stop gnovisor to leave this state")
	<-ctx.Done()
	return context.Canceled
}

// maybeRolling installs a newer rolling release of the running range inside
// rolling_window (decisions D3 and D6, section 7.7).
func (s *Supervisor) maybeRolling(ctx context.Context, height int64) error {
	if s.node == nil {
		return nil
	}
	if done, err := s.maybeLocalRolling(ctx, height); done {
		return err
	}
	if s.ledger == nil {
		return nil
	}
	r, i, ok := s.ledger.RangeOf(s.current.Version)
	if !ok || i != len(s.ledger.BlockRanges())-1 {
		return nil // not on the live range, for example while syncing
	}
	newest := r.Newest()
	if semver.Compare(newest, s.current.Version) <= 0 {
		return nil
	}
	dir, ok := s.rollingDir[newest]
	if !ok {
		e, _ := s.ledger.Entry(newest)
		var err error
		if dir, err = prepare.Version(ctx, s.Layout, e, s.prepOpts()); err != nil {
			s.logf("preparing rolling release %s: %v", newest, err)
			return nil
		}
		s.rollingDir[newest] = dir
		s.logf("rolling release %s ready, installed inside %s", newest, s.Cfg.RollingWindow)
	}
	if !s.Cfg.RollingWindow.Contains(time.Now()) {
		return nil
	}
	info, err := layout.Verify(dir)
	if err != nil {
		s.logf("rolling release %s: %v", newest, err)
		return nil
	}
	return s.installRolling(ctx, height, dir, info)
}

// maybeLocalRolling installs the newest rolling version added by hand that
// is above the running one on the same MAJOR.MINOR: inside rolling_window,
// or at once when it was added with -now. done is true when it acted.
func (s *Supervisor) maybeLocalRolling(ctx context.Context, height int64) (done bool, err error) {
	f, err := local.Read(s.Layout)
	if err != nil {
		s.logf("%v", err)
		return false, nil
	}
	var best *local.Entry
	for _, e := range f.Rolling() {
		e := e
		if semver.Compare(e.Version, s.current.Version) <= 0 || semver.MajorMinor(e.Version) != semver.MajorMinor(s.current.Version) {
			continue
		}
		if best == nil || semver.Compare(e.Version, best.Version) > 0 {
			best = &e
		}
	}
	if best == nil || (!best.Now && !s.Cfg.RollingWindow.Contains(time.Now())) {
		return false, nil
	}
	dir, err := s.localDir(*best)
	if err != nil {
		s.logf("rolling version %s added by hand: %v", best.Version, err)
		return false, nil
	}
	info, _ := layout.ReadInfo(dir)
	return true, s.installRolling(ctx, height, dir, info)
}

// installRolling switches a running node to a version that serves the same
// blocks, with no halt.
func (s *Supervisor) installRolling(ctx context.Context, height int64, dir string, info layout.Info) error {
	s.logf("installing rolling release %s at height %d", info.Version, height)
	from := s.current.Version
	s.stopNode()
	if s.Cfg.Backup {
		dst, err := backup.Make(s.Layout.Home, s.Cfg.BackupDir, backup.Name(height, from))
		if err != nil {
			s.logf("backup failed, no switch, node left stopped: %v", err)
			return s.blocked(ctx)
		}
		s.logf("backup done in %s", dst)
	}
	if err := s.Layout.Switch(dir); err != nil {
		s.logf("switch failed, node left stopped: %v", err)
		return s.blocked(ctx)
	}
	s.logf("switched from %s to %s", from, info.Version)
	s.current, s.currentDir = info, dir
	return s.restartAndWait(ctx, height+1, 0)
}
