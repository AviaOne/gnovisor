// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Command gnovisor runs a gno.land node and takes it through coordinated
// upgrades, past halts met while syncing, and rolling releases.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/AviaOne/gnovisor/internal/config"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/prepare"
	"github.com/AviaOne/gnovisor/internal/rpc"
	"github.com/AviaOne/gnovisor/internal/semver"
	"github.com/AviaOne/gnovisor/internal/supervisor"
	"github.com/AviaOne/gnovisor/internal/version"
)

const usage = version.Attribution + `

usage: gnovisor <command> [flags]

commands:
  init     -home <node dir> -chain-id <id> -version <vX.Y.Z> [-ledger-url <url|file>]
           (or -binary <gnoland> -gnoroot <dir> instead of -version)
  run      -home <node dir>     run and supervise the node (systemd ExecStart)
  status   -home <node dir>     version in service, programmed halt, prepared version, last halt
  prepare  -home <node dir>     prepare the version of the programmed halt now
  add-upgrade -home <node dir> -version <vX.Y.Z> -binary <gnoland> -gnoroot <dir> (-halt <height> | -rolling [-now])
                                add a version received outside the ledger (a private security fix)
  version                       print the version of gnovisor
`

func main() {
	if len(os.Args) < 2 {
		_, _ = fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "prepare":
		err = cmdPrepare(os.Args[2:])
	case "add-upgrade":
		err = cmdAddUpgrade(os.Args[2:])
	case "version":
		fmt.Print(versionText())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		_, _ = fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		logf("error: %v", err)
		os.Exit(1)
	}
}

// versionText is the output of `gnovisor version`: the version, then the
// Appropriate Legal Notices that NOTICE.md asks every copy to keep.
func versionText() string {
	return "gnovisor version: " + version.Version + "\n" +
		version.Attribution + "\n" +
		"License: GNU AGPL-3.0, with additional terms in NOTICE.md\n"
}

// logf writes one GnoVisor line, UTC timestamp, prefixed so it stands apart
// from the node's own output (specification, section 11).
func logf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "gnovisor %s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func homeFlag(fs *flag.FlagSet) *string {
	return fs.String("home", "", "node directory (the --data-dir of gnoland)")
}

func absHome(h string) (string, error) {
	if h == "" {
		return "", errors.New("-home is required")
	}
	return filepath.Abs(h)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	home := homeFlag(fs)
	_ = fs.Parse(args)
	h, err := absHome(*home)
	if err != nil {
		return err
	}
	cfg, err := config.Load(h)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	s := &supervisor.Supervisor{Cfg: cfg, Layout: layout.New(h), Platform: platform(), Log: logf}
	return s.Run(ctx)
}

func loadLedger(ctx context.Context, src, chainID string) (*ledger.Ledger, error) {
	var data []byte
	var err error
	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if rerr != nil {
			return nil, rerr
		}
		c := &http.Client{Timeout: time.Minute}
		resp, rerr := c.Do(req)
		if rerr != nil {
			return nil, rerr
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: HTTP %d", src, resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	} else {
		data, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, err
	}
	l, err := ledger.Parse(data)
	if err != nil {
		return nil, err
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("ledger %s is invalid:\n%w", src, err)
	}
	if l.ChainID != chainID {
		return nil, fmt.Errorf("ledger %s is for chain %q, not %q", src, l.ChainID, chainID)
	}
	return l, nil
}

// cmdInit creates gnovisor/ with the genesis version, the one the node runs
// today. With -version, GnoVisor takes that version itself: the binary from
// the gnoland image the ledger names, and the GNOROOT cloned at its tag.
// With -binary and -gnoroot, it takes files the operator already has: the
// version is found in the ledger by the binary's sha256 (a release binary),
// and the GNOROOT must be a git checkout at that version's commit (D1).
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	home := homeFlag(fs)
	ver := fs.String("version", "", "version the node runs today, vX.Y.Z: gnovisor fetches it")
	bin := fs.String("binary", "", "instead of -version: a release gnoland binary the node runs")
	root := fs.String("gnoroot", "", "with -binary: GNOROOT of that binary, a git checkout of gnolang/gno")
	chainID := fs.String("chain-id", "", "chain id, for example gnoland-1")
	ledgerURL := fs.String("ledger-url", "", "upgrades.json URL or file (default: the chain's ledger on gnolang/gno)")
	_ = fs.Parse(args)
	h, err := absHome(*home)
	if err != nil {
		return err
	}
	switch {
	case *chainID == "":
		return errors.New("-chain-id is required")
	case *ver != "" && (*bin != "" || *root != ""):
		return errors.New("give -version, or -binary and -gnoroot, not both")
	case *ver == "" && (*bin == "" || *root == ""):
		return errors.New("give -version, or -binary and -gnoroot")
	}
	lay := layout.New(h)
	if _, err := os.Lstat(lay.Root); err == nil {
		return fmt.Errorf("%s already exists, nothing done", lay.Root)
	}
	src := *ledgerURL
	if src == "" {
		src = config.LedgerURLFor(*chainID)
		if src == "" {
			return fmt.Errorf("no default ledger for chain %s: pass -ledger-url", *chainID)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	l, err := loadLedger(ctx, src, *chainID)
	if err != nil {
		return err
	}
	if *ver != "" {
		return initFromLedger(ctx, lay, l, *ver, h, *chainID)
	}
	sum, err := layout.SHA256File(*bin)
	if err != nil {
		return err
	}
	e, ok := l.FindBySHA256(platform(), sum)
	if !ok {
		return fmt.Errorf("%s (sha256 %s) is not a %s binary of the ledger", *bin, sum, platform())
	}
	out, err := exec.Command("git", "-C", *root, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("%s is not a git checkout: %w", *root, err)
	}
	if got := strings.TrimSpace(string(out)); got != e.Commit {
		return fmt.Errorf("%s is at commit %s, %s is commit %s", *root, got, e.Version, e.Commit)
	}

	tmp := lay.Root + ".init-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	gen := filepath.Join(tmp, "genesis")
	if err := os.MkdirAll(filepath.Join(gen, "bin"), 0o755); err != nil {
		return err
	}
	if err := copyTree(*bin, layout.Binary(gen)); err != nil {
		return err
	}
	if err := os.Chmod(layout.Binary(gen), 0o755); err != nil {
		return err
	}
	if err := copyTree(*root, layout.Gnoroot(gen)); err != nil {
		return err
	}
	if err := layout.WriteInfo(gen, layout.Info{Version: e.Version, Commit: e.Commit, SHA256: sum}); err != nil {
		return err
	}
	return finishInit(tmp, lay, h, *chainID, e.Version, sum)
}

// Where `init -version` fetches from; replaced by the tests only.
var (
	initRegistry    = ""
	initClient      *http.Client
	initGnorootRepo = config.DefaultGnorootRepo
)

// initFromLedger prepares the genesis version from the ledger alone.
func initFromLedger(ctx context.Context, lay layout.Layout, l *ledger.Ledger, ver, h, chainID string) error {
	e, ok := l.Entry(ver)
	if !ok {
		return fmt.Errorf("%s is not in the ledger of %s", ver, chainID)
	}
	tmp := lay.Root + ".init-tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	info, err := prepare.Into(ctx, filepath.Join(tmp, "genesis"), e, prepare.Options{
		Platform: platform(), AutoDownload: true, GnorootRepo: initGnorootRepo,
		HTTPClient: initClient, Registry: initRegistry, Log: logf,
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return finishInit(tmp, lay, h, chainID, e.Version, info.SHA256)
}

// finishInit adds current and gnovisor.toml to tmp and moves it in place.
func finishInit(tmp string, lay layout.Layout, h, chainID, ver, sum string) error {
	if err := os.Symlink("genesis", filepath.Join(tmp, "current")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, config.FileName), []byte(config.Template(h, chainID)), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, lay.Root); err != nil {
		return err
	}
	logf("initialised %s with %s (%s)", lay.Root, ver, sum)
	logf("complete %s, rolling_window and node_args first", filepath.Join(lay.Root, config.FileName))
	return nil
}

// copyTree copies a file or a directory, keeping modes and symlinks.
func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return copyFile(src, dst, st.Mode().Perm())
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		t := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(t, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, t)
		case info.Mode().IsRegular():
			return copyFile(p, t, info.Mode().Perm())
		default:
			return fmt.Errorf("%s: unsupported file type", p)
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	home := homeFlag(fs)
	_ = fs.Parse(args)
	h, err := absHome(*home)
	if err != nil {
		return err
	}
	cfg, err := config.Load(h)
	if err != nil {
		return err
	}
	lay := layout.New(h)
	dir, err := lay.CurrentDir()
	if err != nil {
		return err
	}
	info, err := layout.Verify(dir)
	if err != nil {
		return err
	}
	st, err := lay.ReadState()
	if err != nil {
		return err
	}
	fmt.Printf("version in service: %s (sha256 %s)\n", info.Version, info.SHA256)
	fmt.Printf("last halt handled:  %d\n", st.LastHalt)
	if st.Pending != nil {
		fmt.Printf("pending halt:       %d, the node stays stopped until its version is ready\n", st.Pending.Height)
	}
	lf, err := local.Read(lay)
	if err != nil {
		return err
	}
	for _, e := range lf.Upgrades {
		switch {
		case e.HaltHeight > 0:
			fmt.Printf("added by hand:      %s for the halt at height %d\n", e.Version, e.HaltHeight)
		case e.Now:
			fmt.Printf("added by hand:      %s, rolling, at once\n", e.Version)
		default:
			fmt.Printf("added by hand:      %s, rolling, inside rolling_window\n", e.Version)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := rpc.New(cfg.RPC)
	s, err := c.Status(ctx)
	if err != nil {
		fmt.Printf("node:               not answering (%v)\n", err)
		return nil
	}
	fmt.Printf("node height:        %d\n", s.Height)
	halt, err := c.Halt(ctx)
	if err != nil {
		return err
	}
	if halt.Height <= s.Height {
		fmt.Println("programmed halt:    none")
		return nil
	}
	fmt.Printf("programmed halt:    %d, floor %q\n", halt.Height, halt.MinVersion)
	l, err := loadLedger(ctx, cfg.LedgerURL, cfg.ChainID)
	if err != nil {
		fmt.Printf("ledger:             %v\n", err)
		return nil
	}
	e, ok := l.UpgradeAt(halt.Height)
	switch {
	case !ok:
		fmt.Println("prepared version:   none, no ledger entry for this halt yet")
	case lay.VersionDir(e.Version) != "":
		fmt.Printf("prepared version:   %s\n", e.Version)
	default:
		fmt.Printf("prepared version:   none, %s not prepared yet\n", e.Version)
	}
	return nil
}

func cmdPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	home := homeFlag(fs)
	_ = fs.Parse(args)
	h, err := absHome(*home)
	if err != nil {
		return err
	}
	cfg, err := config.Load(h)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	c := rpc.New(cfg.RPC)
	s, err := c.Status(ctx)
	if err != nil {
		return err
	}
	halt, err := c.Halt(ctx)
	if err != nil {
		return err
	}
	if halt.Height <= s.Height {
		return errors.New("no halt programmed")
	}
	l, err := loadLedger(ctx, cfg.LedgerURL, cfg.ChainID)
	if err != nil {
		return err
	}
	e, ok := l.UpgradeAt(halt.Height)
	if !ok {
		return fmt.Errorf("no ledger entry for the halt at height %d yet", halt.Height)
	}
	dir, err := prepare.Version(ctx, layout.New(h), e, prepare.Options{
		Platform: platform(), AutoDownload: cfg.AutoDownload, GnorootRepo: cfg.GnorootRepo, Log: logf,
	})
	if err != nil {
		return err
	}
	logf("%s ready in %s for the halt at height %d", e.Version, dir, halt.Height)
	return nil
}

// cmdAddUpgrade adds a version received outside the ledger, such as a
// security fix handed to validators privately before the flaw is disclosed
// (decision D9). The binary and the GNOROOT are copied into
// upgrades/<version>/ and recorded with their sha256 in local-upgrades.json.
// Nothing published can confirm them: the operator vouches for them. The
// GNOROOT is not checked against Git, since a private fix may have no
// public tag.
func cmdAddUpgrade(args []string) error {
	fs := flag.NewFlagSet("add-upgrade", flag.ExitOnError)
	home := homeFlag(fs)
	ver := fs.String("version", "", "version of the binary, vMAJOR.MINOR.PATCH")
	bin := fs.String("binary", "", "the gnoland binary received")
	root := fs.String("gnoroot", "", "the GNOROOT of that binary")
	halt := fs.Int64("halt", 0, "the coordinated halt this version runs after")
	rolling := fs.Bool("rolling", false, "a version installed without a halt, on the same MAJOR.MINOR")
	now := fs.Bool("now", false, "with -rolling: install at the next poll instead of inside rolling_window")
	_ = fs.Parse(args)
	h, err := absHome(*home)
	if err != nil {
		return err
	}
	switch {
	case *ver == "" || *bin == "" || *root == "":
		return errors.New("-version, -binary and -gnoroot are required")
	case !semver.IsValid(*ver) || semver.Canonical(*ver) != *ver:
		return fmt.Errorf("version %q is not of the form vMAJOR.MINOR.PATCH", *ver)
	case *halt < 0:
		return errors.New("-halt must be positive")
	case (*halt > 0) == *rolling:
		return errors.New("give exactly one of -halt <height> or -rolling")
	case *now && !*rolling:
		return errors.New("-now goes with -rolling")
	}
	lay := layout.New(h)
	curDir, err := lay.CurrentDir()
	if err != nil {
		return fmt.Errorf("no current version: %w (run gnovisor init)", err)
	}
	cur, err := layout.ReadInfo(curDir)
	if err != nil {
		return err
	}
	if *rolling && (semver.Compare(*ver, cur.Version) <= 0 || semver.MajorMinor(*ver) != semver.MajorMinor(cur.Version)) {
		return fmt.Errorf("a rolling version must be above %s on the same MAJOR.MINOR (%s)", cur.Version, semver.MajorMinor(cur.Version))
	}
	f, err := local.Read(lay)
	if err != nil {
		return err
	}
	if f.Has(*ver) {
		return fmt.Errorf("%s is already in %s", *ver, local.Path(lay))
	}
	if e, ok := f.AtHalt(*halt); ok {
		return fmt.Errorf("%s is already added for the halt at height %d", e.Version, *halt)
	}
	if _, err := os.Lstat(lay.Upgrade(*ver)); err == nil {
		return fmt.Errorf("%s already exists, nothing done", lay.Upgrade(*ver))
	}
	if st, err := os.Stat(filepath.Join(*root, "gnovm", "stdlibs")); err != nil || !st.IsDir() {
		return fmt.Errorf("%s has no gnovm/stdlibs: not a GNOROOT", *root)
	}
	if err := os.MkdirAll(lay.Upgrades(), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(lay.Upgrades(), "."+*ver+".add-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.MkdirAll(filepath.Join(tmp, "bin"), 0o755); err != nil {
		return err
	}
	if err := copyTree(*bin, layout.Binary(tmp)); err != nil {
		return err
	}
	if err := os.Chmod(layout.Binary(tmp), 0o755); err != nil {
		return err
	}
	if err := copyTree(*root, layout.Gnoroot(tmp)); err != nil {
		return err
	}
	sum, err := layout.SHA256File(layout.Binary(tmp))
	if err != nil {
		return err
	}
	if err := layout.WriteInfo(tmp, layout.Info{Version: *ver, SHA256: sum}); err != nil {
		return err
	}
	if err := os.Rename(tmp, lay.Upgrade(*ver)); err != nil {
		return err
	}
	f.Upgrades = append(f.Upgrades, local.Entry{Version: *ver, HaltHeight: *halt, Now: *now, SHA256: sum, Added: time.Now().UTC()})
	if err := local.Write(lay, f); err != nil {
		return err
	}
	if *rolling {
		logf("added %s (sha256 %s), rolling, installed %s", *ver, sum, map[bool]string{true: "at the next poll", false: "inside rolling_window"}[*now])
	} else {
		logf("added %s (sha256 %s) for the halt at height %d", *ver, sum, *halt)
	}
	return nil
}
