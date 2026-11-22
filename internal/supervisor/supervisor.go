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
//     an upgrade;
//   - only a vote stops the chain (decision D15): a halt height the ledger
//     announces with no vote behind it is not a halt, and the node goes on;
//   - since 2026-10-29 gno.land writes the ledger entry of a coordinated
//     upgrade only once the chain runs the release, after the halt
//     (gno.land/adr/pr6381_ledger_entry_after_upgrade.md), and every halt
//     proposal sets the version released as halt_min_version (RELEASING.md):
//     the vote names the version (decision D18);
//   - a node catching up in block sync does not stay up at a halt: it exits
//     (tm2/pkg/bft/blockchain/reactor.go, a panic with no recover), often
//     before any poll has read the halt height; exit.go takes it across.
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
	"github.com/AviaOne/gnovisor/internal/docker"
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
	prepTried   map[string]time.Time
	rollingDir  map[string]string
	warnedLocal int64
	// chainHalt is the halt the node last reported over RPC, kept for an
	// exit at a halt that no later poll can read.
	chainHalt rpc.Halt
	// lastExit is the last exit handled by nodeExited, with the version that
	// ran, so that the same exit twice is not acted on again.
	lastExit string
}

// errNodeExited is returned when the node exits by itself before its RPC
// answers.
var errNodeExited = errors.New("node exited by itself")

// Where the version run after a voted halt comes from, in the order GnoVisor
// takes it.
const (
	SourceHand   = "added by hand"
	SourceLedger = "ledger"
	SourceVote   = "named by the vote"
)

// VersionForHalt returns the version to run after the voted halt at h, and
// where it comes from: a version added by hand wins (decision D19), then the
// ledger's entry for that halt, then the version the vote set as its floor
// (decision D18). It returns "" when none is known.
func VersionForHalt(l *ledger.Ledger, f local.File, h int64, floor string) (string, string) {
	if e, ok := f.AtHalt(h); ok {
		return e.Version, SourceHand
	}
	if l != nil {
		if e, ok := l.UpgradeAt(h); ok {
			return e.Version, SourceLedger
		}
	}
	if e, ok := ledger.Voted(floor, h); ok {
		return e.Version, SourceVote
	}
	return "", ""
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
		Docker:       s.docker(),
	}
}

// docker is the docker client in Docker mode, nil for a binary.
func (s *Supervisor) docker() *docker.Client {
	if !s.Cfg.Docker() {
		return nil
	}
	return &docker.Client{Bin: s.Cfg.DockerBin}
}

// Run supervises until ctx is cancelled (SIGTERM or SIGINT) or the node
// exits by itself. It returns nil on a requested stop.
func (s *Supervisor) Run(ctx context.Context) error {
	s.rpc = rpc.New(s.Cfg.RPC)
	s.prepared = map[int64]bool{}
	s.prepTried = map[string]time.Time{}
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
		if err := s.handleHalt(ctx, s.state.Pending.Height, s.state.Pending.MinVersion); err != nil {
			if errors.Is(err, context.Canceled) {
				s.stopNode()
				return nil
			}
			return err
		}
	} else if err := s.start(); err != nil {
		return err
	}
	for {
		err := s.checkChainID(ctx)
		if errors.Is(err, errNodeExited) {
			// It may have refused to start, or exited at a halt while
			// catching up (exit.go).
			var handled bool
			if handled, err = s.nodeExited(ctx); handled && err == nil {
				continue
			}
			if !handled {
				err = fmt.Errorf("node exited by itself: %s", s.node.exitDescription())
			}
		}
		if err != nil {
			s.stopNode()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		break
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
				handled, err := s.nodeExited(ctx)
				if handled {
					if err != nil {
						if errors.Is(err, context.Canceled) {
							s.stopNode()
							return nil
						}
						return err
					}
					continue
				}
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
	if info.Docker() != s.Cfg.Docker() {
		return fmt.Errorf("current version %s is prepared for runtime %q, gnovisor.toml says %q", info.Version, map[bool]string{true: config.RuntimeDocker, false: config.RuntimeBinary}[info.Docker()], s.Cfg.Runtime)
	}
	s.current, s.currentDir = info, dir
	return nil
}

func (s *Supervisor) start() error {
	var n *node
	var err error
	if d := s.docker(); d != nil {
		// A container left by a GnoVisor that ended without stopping it
		// would keep the name, and a second gnoland on the same node
		// directory: it is removed first.
		if err := d.Remove(context.Background(), s.Cfg.Container); err != nil {
			return fmt.Errorf("removing container %s: %w", s.Cfg.Container, err)
		}
		n, err = startContainer(d, s.Cfg.Container, s.current.ImageRef, s.Layout.Home, s.Cfg.DockerArgs, s.Cfg.NodeArgs)
	} else {
		n, err = startNode(s.currentDir, s.Layout.Home, s.Cfg.NodeArgs)
	}
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
				return fmt.Errorf("%w: %s", errNodeExited, s.node.exitDescription())
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
	s.chainHalt = halt

	scheduled := halt.Height > st.Height
	if scheduled && !s.prepared[halt.Height] {
		s.prepareAhead(ctx, halt)
	}

	if h := st.Height; h > 0 && h != s.state.LastHalt {
		// A halt is a vote on the chain (decision D15): the chain stops only
		// where GovDAO set halt_height, and the ledger only names the version
		// that runs after it. A ledger entry, or a version added by hand, at
		// a height no vote set is no halt: the chain goes on, and so does the
		// node. The one exception is a halt the ledger records as past
		// (halt_time is null until the halt has happened,
		// gno.land/pkg/upgrades/ledger.go, Entry): a node replaying the
		// chain stops there, whatever halt the chain parameter names today.
		// At a halt, block h is committed and the node produces nothing more
		// (ADR pr5368): there is nothing to confirm, and waiting would only
		// lengthen the outage (G12). halt_confirm is kept for a node that
		// stops elsewhere.
		if halt.Height == h || s.pastLedgerHalt(h) {
			s.logf("halt reached at height %d", h)
			floor := ""
			if halt.Height == h {
				floor = halt.MinVersion
			}
			return s.handleHalt(ctx, h, floor)
		}
		if time.Since(s.lastChange) >= s.Cfg.HaltConfirm {
			if s.warnedLocal != h {
				s.warnedLocal = h
				s.logf("node stays at height %d, which is neither a halt voted on the chain nor a past halt of the ledger: not an upgrade, nothing done", h)
			}
			return nil
		}
	}

	if err := s.maybeLedgerFix(ctx, st.Height); err != nil {
		return err
	}
	if !scheduled && !st.CatchingUp {
		return s.maybeRolling(ctx, st.Height)
	}
	return nil
}

// tried reports whether a preparation of version failed less than
// PendingLedgerInterval ago, so that a version not yet published is looked
// for once a minute, not at every poll.
func (s *Supervisor) tried(version string) bool {
	t, ok := s.prepTried[version]
	return ok && time.Since(t) < PendingLedgerInterval
}

// prepareAhead prepares the version of the voted halt, before it is reached,
// from the source VersionForHalt gives.
func (s *Supervisor) prepareAhead(ctx context.Context, halt rpc.Halt) {
	h := halt.Height
	if loc, ok := s.localAt(h); ok {
		if _, err := s.localDir(loc); err != nil {
			s.logf("version %s added by hand for the halt at height %d: %v", loc.Version, h, err)
			return
		}
		s.prepared[h] = true
		s.logf("ready for the halt at height %d, version %s added by hand", h, loc.Version)
		return
	}
	var e *ledger.Entry
	how := ""
	if s.ledger != nil {
		e, _ = s.ledger.UpgradeAt(h)
	}
	if e == nil {
		if v, ok := ledger.Voted(halt.MinVersion, h); ok {
			e, how = v, " named by the vote"
		}
	}
	if e == nil {
		if time.Since(s.ledgerAt) < s.Cfg.PollInterval {
			// Once per ledger read (section 7.2).
			s.logf("halt programmed at height %d, with no version in the vote and no ledger entry yet", h)
		}
		return
	}
	if s.tried(e.Version) {
		return
	}
	if _, err := prepare.Version(ctx, s.Layout, e, s.prepOpts()); err != nil {
		s.prepTried[e.Version] = time.Now()
		s.logf("preparing %s for the halt at height %d: %v", e.Version, h, err)
		return
	}
	s.prepared[h] = true
	s.logf("ready for the halt at height %d, version %s%s", h, e.Version, how)
}

// maybeLedgerFix follows the ledger written after the last halt (decision
// D21): when it names, for the blocks after that halt, a newer version than
// the one in service, gno.land runs that one, such as a fix of a version
// that produces no block, and GnoVisor switches to it. The vote only sets a
// floor, and the node itself refuses a version below it. A version added by
// hand for that halt is never replaced (decision D19).
func (s *Supervisor) maybeLedgerFix(ctx context.Context, height int64) error {
	h := s.state.LastHalt
	if h == 0 || s.ledger == nil || s.node == nil {
		return nil
	}
	r, ok := s.target(h)
	if !ok || r.Serves(s.current.Version) {
		return nil
	}
	if _, ok := s.localAt(h); ok {
		return nil
	}
	newest := r.Newest()
	if semver.Compare(newest, s.current.Version) <= 0 || s.tried(newest) {
		return nil
	}
	e, _ := s.ledger.Entry(newest)
	dir, err := prepare.Version(ctx, s.Layout, e, s.prepOpts())
	if err == nil {
		var info layout.Info
		if info, err = layout.Verify(dir); err == nil {
			s.logf("the ledger names %s for the blocks after %d, not %s: switching", newest, h, s.current.Version)
			return s.switchTo(ctx, h, dir, info, height+1)
		}
	}
	s.prepTried[newest] = time.Now()
	s.logf("the ledger names %s for the blocks after %d, not ready: %v", newest, h, err)
	return nil
}

// pastLedgerHalt reports whether the ledger records a halt at h that has
// already happened on the chain.
func (s *Supervisor) pastLedgerHalt(h int64) bool {
	if s.ledger == nil {
		return false
	}
	e, ok := s.ledger.UpgradeAt(h)
	return ok && e.HaltTime != nil
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
// The node may be running (halted) or already stopped (pending halt). floor
// is the halt_min_version the vote set, "" for a past halt of the ledger.
// The version run is, in order: the one added by hand for this halt (D19),
// the one in service when it is newer than the ledger's and meets the floor
// (D26), the one the ledger names for the blocks after it, the one the vote
// names (D18).
func (s *Supervisor) handleHalt(ctx context.Context, h int64, floor string) error {
	if err := s.setPending(h, floor); err != nil {
		return err
	}
	// The ledger is read again now: the one in memory can be up to
	// LedgerInterval old, and gno.land may have named another version for
	// this halt in the meantime (a last-minute fix is a new tag).
	s.refreshLedger(ctx, true)
	for {
		r, ok := s.target(h)
		if loc, hasLoc := s.localAt(h); hasLoc {
			dir, err := s.localDir(loc)
			if err == nil {
				if ok && r.Newest() != loc.Version {
					s.logf("halt at height %d: the ledger gives %s, the version added by hand, %s, is the one run", h, r.Newest(), loc.Version)
				}
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
		var e *ledger.Entry
		switch v, voted := ledger.Voted(floor, h); {
		case ok && r.Serves(s.current.Version), !ok && voted && v.Version == s.current.Version:
			// Already on a version that serves the next blocks: a restart
			// is enough (section 7.4).
			s.logf("version %s serves the blocks after %d, restarting the node", s.current.Version, h)
			return s.switchTo(ctx, h, s.currentDir, s.current, h+1)
		case ok && s.newerServes(h, r, floor):
			s.logf("version %s is newer than %s, the version the ledger gives for the blocks after %d, and meets the floor of that halt: kept, restarting the node", s.current.Version, r.Newest(), h)
			return s.switchTo(ctx, h, s.currentDir, s.current, h+1)
		case ok:
			e, _ = s.ledger.Entry(r.Newest())
		case voted:
			s.logf("no ledger entry for the halt at height %d: version %s named by the vote", h, v.Version)
			e = v
		}
		if e != nil {
			dir, err := prepare.Version(ctx, s.Layout, e, s.prepOpts())
			if err == nil {
				info, err := layout.Verify(dir)
				if err == nil {
					return s.switchTo(ctx, h, dir, info, h+1)
				}
			}
			s.logf("version %s for the halt at height %d is not ready: %v", e.Version, h, err)
		} else {
			s.logf("no version for the halt at height %d, neither in the vote nor in the ledger: the node stays stopped, the old binary is never restarted", h)
		}
		if err := s.waitPending(ctx, h); err != nil {
			return err
		}
	}
}

// newerServes reports whether the version in service may run the blocks
// after the halt at h although the ledger gives an older one for them
// (decision D26). gno.land syncs a node from genesis on a recent version and
// restarts it on that same version at every past halt whose floor it meets
// (misc/deployments/mainnet.gno.land/UPGRADES.md, "Joining from genesis",
// branch chain/mainnet): taking the older version of the ledger instead would
// run it on a database a newer version already wrote. The version in service
// must be newer than the ledger's and meet both the floor the vote set and
// the one the ledger records for that halt; otherwise the ledger's version
// runs, as before.
func (s *Supervisor) newerServes(h int64, r ledger.Range, floor string) bool {
	cur := s.current.Version
	if s.ledger == nil || semver.Compare(cur, r.Newest()) <= 0 {
		return false
	}
	floors := []string{floor}
	if e, ok := s.ledger.UpgradeAt(h); ok && e.HaltMinVersion != nil {
		floors = append(floors, *e.HaltMinVersion)
	}
	for _, f := range floors {
		if f != "" && semver.Compare(cur, f) < 0 {
			return false
		}
	}
	return true
}

// waitPending records the halt as pending, keeps the node stopped, waits
// PendingLedgerInterval and reads the ledger again. The file of versions
// added by hand is read at every pass, so `gnovisor add-upgrade` run during
// the wait is taken within a minute.
func (s *Supervisor) waitPending(ctx context.Context, h int64) error {
	if err := s.setPending(h, ""); err != nil {
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
	if info.Docker() != s.Cfg.Docker() {
		return "", fmt.Errorf("%s was added for runtime %q, gnovisor.toml says %q", e.Version, map[bool]string{true: config.RuntimeDocker, false: config.RuntimeBinary}[info.Docker()], s.Cfg.Runtime)
	}
	if info.Docker() {
		if info.ImageRef != e.Image {
			return "", fmt.Errorf("%s has image %s, add-upgrade recorded %s", dir, info.ImageRef, e.Image)
		}
		return dir, nil
	}
	if info.SHA256 != e.SHA256 {
		return "", fmt.Errorf("%s has sha256 %s, add-upgrade recorded %s", dir, info.SHA256, e.SHA256)
	}
	return dir, nil
}

// setPending records the halt at h as pending, with the floor the vote set;
// an empty floor keeps the one already recorded for that halt.
func (s *Supervisor) setPending(h int64, floor string) error {
	if p := s.state.Pending; p != nil && p.Height == h && (floor == "" || floor == p.MinVersion) {
		return nil
	}
	s.state.Pending = &layout.Pending{Height: h, MinVersion: floor}
	return s.Layout.WriteState(s.state)
}

// switchTo stops the node, backs it up, points current at dir, records the
// halt, restarts and waits for block wantHeight. On failure the node is
// left stopped and GnoVisor never goes back to the old version: the backup
// and the old version directory stay for a manual intervention.
// A switch, once started, is not interrupted by a stop request.
func (s *Supervisor) switchTo(ctx context.Context, h int64, dir string, info layout.Info, wantHeight int64) error {
	if err := s.setPending(h, ""); err != nil {
		return err
	}
	from := s.current.Version
	s.stopNode()
	switch {
	case s.Cfg.Backup && s.pastLedgerHalt(h):
		// The network crossed this halt long ago and the ledger records it:
		// the node is catching up, its data can be fetched again, and a copy
		// of the whole database at every past halt could fill the disk
		// (decision D23).
		s.logf("no backup: the halt at height %d is past for the network (ledger halt_time)", h)
	case s.Cfg.Backup:
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
// after a halt, a version that exits is followed by a watch for its fix, and
// a node that produces no block within restart_timeout keeps running
// (decision D20): the chain may stay halted long after the switch, as on
// onyx-1 on 2026-10-29, and the node catches up when it resumes; a fix the
// ledger names later is taken by maybeLedgerFix (decision D21).
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
			// A node catching up may reach the next halt, or be refused, before
			// any block is read here (exit.go).
			if handled, err := s.nodeExited(ctx); handled {
				return err
			}
			s.logf("node exited after the switch (%s), left stopped, no return to the old version", s.node.exitDescription())
			s.node = nil
			return failed()
		default:
		}
		if st, err := s.rpc.Status(context.Background()); err == nil && st.Height >= wantHeight {
			s.lastHeight, s.lastChange = st.Height, time.Now()
			s.lastExit = ""
			s.logf("block %d seen, update succeeded", st.Height)
			return nil
		}
		// A stop request during the wait is honoured: the switch itself is
		// already recorded.
		if err := s.sleep(ctx, s.Cfg.PollInterval); err != nil {
			return err
		}
	}
	if halt > 0 {
		s.logf("no block %d within %s: the node keeps running on %s and catches up when the chain resumes", wantHeight, s.Cfg.RestartTimeout, s.current.Version)
		return nil
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
	if err := s.setPending(h, ""); err != nil {
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
