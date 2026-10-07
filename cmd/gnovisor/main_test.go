// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/AviaOne/gnovisor/internal/config"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/testutil"
)

func TestInitWithVersionTakesTheImageBinary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	f := testutil.NewFixture(t, "test-1",
		&testutil.Release{Kind: "genesis", Version: "v1.5.0", Binary: []byte("bin-1.5.0")},
	)
	ledgerFile := filepath.Join(t.TempDir(), "upgrades.json")
	if err := os.WriteFile(ledgerFile, f.Ledger(-1), 0o644); err != nil {
		t.Fatal(err)
	}
	initRegistry, initClient, initGnorootRepo = f.Server.URL, f.Client(), f.Repo
	t.Cleanup(func() { initRegistry, initClient, initGnorootRepo = "", nil, config.DefaultGnorootRepo })

	home := t.TempDir()
	if err := cmdInit([]string{"-home", home, "-version", "v1.4.0", "-chain-id", "test-1", "-ledger-url", ledgerFile}); err == nil {
		t.Fatal("a version the ledger does not list must be refused")
	}
	if err := cmdInit([]string{"-home", home, "-version", "v1.5.0", "-binary", "x", "-chain-id", "test-1", "-ledger-url", ledgerFile}); err == nil {
		t.Fatal("-version with -binary must be refused")
	}
	if err := cmdInit([]string{"-home", home, "-version", "v1.5.0", "-chain-id", "test-1", "-ledger-url", ledgerFile}); err != nil {
		t.Fatal(err)
	}
	lay := layout.New(home)
	info, err := layout.Verify(lay.Genesis())
	if err != nil || info.Version != "v1.5.0" || info.Image != f.Releases[0].ImageDigest {
		t.Fatalf("%+v %v", info, err)
	}
	if b, _ := os.ReadFile(layout.Binary(lay.Genesis())); string(b) != "bin-1.5.0" {
		t.Fatalf("binary = %q", b)
	}
	if _, err := os.Stat(filepath.Join(lay.Root, config.FileName)); err != nil {
		t.Fatal("template not written")
	}
}

func initFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	f := testutil.NewFixture(t, "test-1",
		&testutil.Release{Kind: "genesis", Version: "v1.5.0", Binary: []byte("bin-1.5.0")},
		&testutil.Release{Kind: "upgrade", Version: "v1.6.0", HaltHeight: 100, Binary: []byte("bin-1.6.0")},
	)
	dir := t.TempDir()
	ledgerFile := filepath.Join(dir, "upgrades.json")
	if err := os.WriteFile(ledgerFile, f.Ledger(-1), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "gnoland")
	if err := os.WriteFile(bin, []byte("bin-1.5.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "gnoroot")
	if out, err := exec.Command("git", "clone", "--quiet", "--branch", "v1.5.0", f.Repo, root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return ledgerFile, bin, root, f.Repo
}

func TestInitCreatesTheGenesisVersion(t *testing.T) {
	ledgerFile, bin, root, _ := initFixture(t)
	home := t.TempDir()
	if err := cmdInit([]string{"-home", home, "-binary", bin, "-gnoroot", root, "-chain-id", "test-1", "-ledger-url", ledgerFile}); err != nil {
		t.Fatal(err)
	}
	lay := layout.New(home)
	dir, err := lay.CurrentDir()
	if err != nil || dir != lay.Genesis() {
		t.Fatalf("current = %s %v", dir, err)
	}
	info, err := layout.Verify(dir)
	if err != nil || info.Version != "v1.5.0" {
		t.Fatalf("%+v %v", info, err)
	}
	b, err := os.ReadFile(filepath.Join(lay.Root, config.FileName))
	if err != nil || !strings.Contains(string(b), `rolling_window = ""`) {
		t.Fatal("template not written")
	}
	// A second init changes nothing.
	if err := cmdInit([]string{"-home", home, "-binary", bin, "-gnoroot", root, "-chain-id", "test-1", "-ledger-url", ledgerFile}); err == nil {
		t.Fatal("init over an existing gnovisor/ must refuse")
	}
}

func TestInitRefusesAnUnknownBinaryOrAWrongGnoroot(t *testing.T) {
	ledgerFile, bin, root, repo := initFixture(t)
	other := filepath.Join(t.TempDir(), "gnoland")
	_ = os.WriteFile(other, []byte("hand-built"), 0o755)
	err := cmdInit([]string{"-home", t.TempDir(), "-binary", other, "-gnoroot", root, "-chain-id", "test-1", "-ledger-url", ledgerFile})
	if err == nil || !strings.Contains(err.Error(), "is not a") {
		t.Fatalf("err = %v", err)
	}
	wrong := filepath.Join(t.TempDir(), "gnoroot")
	if out, err := exec.Command("git", "clone", "--quiet", "--branch", "v1.6.0", repo, wrong).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	home := t.TempDir()
	err = cmdInit([]string{"-home", home, "-binary", bin, "-gnoroot", wrong, "-chain-id", "test-1", "-ledger-url", ledgerFile})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "gnovisor")); !os.IsNotExist(err) {
		t.Fatal("a refused init must leave nothing")
	}
}

func TestAddUpgrade(t *testing.T) {
	ledgerFile, bin, root, _ := initFixture(t)
	home := t.TempDir()
	if err := cmdInit([]string{"-home", home, "-binary", bin, "-gnoroot", root, "-chain-id", "test-1", "-ledger-url", ledgerFile}); err != nil {
		t.Fatal(err)
	}
	fix := filepath.Join(t.TempDir(), "gnoland-fix")
	_ = os.WriteFile(fix, []byte("security fix"), 0o644)

	refused := map[string][]string{
		"no version":        {"-binary", fix, "-gnoroot", root, "-halt", "500"},
		"bad version":       {"-version", "1.6.0", "-binary", fix, "-gnoroot", root, "-halt", "500"},
		"halt and rolling":  {"-version", "v1.6.0", "-binary", fix, "-gnoroot", root, "-halt", "500", "-rolling"},
		"neither":           {"-version", "v1.6.0", "-binary", fix, "-gnoroot", root},
		"now without roll":  {"-version", "v1.6.0", "-binary", fix, "-gnoroot", root, "-halt", "500", "-now"},
		"rolling not above": {"-version", "v1.5.0", "-binary", fix, "-gnoroot", root, "-rolling"},
		"rolling new minor": {"-version", "v1.6.1", "-binary", fix, "-gnoroot", root, "-rolling"},
		"not a gnoroot":     {"-version", "v1.6.0", "-binary", fix, "-gnoroot", t.TempDir(), "-halt", "500"},
	}
	for name, args := range refused {
		if err := cmdAddUpgrade(append([]string{"-home", home}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if err := cmdAddUpgrade([]string{"-home", home, "-version", "v1.6.0", "-binary", fix, "-gnoroot", root, "-halt", "500"}); err != nil {
		t.Fatal(err)
	}
	lay := layout.New(home)
	info, err := layout.Verify(lay.Upgrade("v1.6.0"))
	if err != nil || info.Version != "v1.6.0" {
		t.Fatalf("%+v %v", info, err)
	}
	st, _ := os.Stat(layout.Binary(lay.Upgrade("v1.6.0")))
	if st.Mode()&0o100 == 0 {
		t.Fatal("binary not executable")
	}
	f, err := local.Read(lay)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := f.AtHalt(500); !ok || e.SHA256 != info.SHA256 {
		t.Fatalf("entry %+v", e)
	}
	if err := cmdAddUpgrade([]string{"-home", home, "-version", "v1.6.0", "-binary", fix, "-gnoroot", root, "-halt", "600"}); err == nil {
		t.Fatal("the same version twice must be refused")
	}
	if err := cmdAddUpgrade([]string{"-home", home, "-version", "v1.7.0", "-binary", fix, "-gnoroot", root, "-halt", "500"}); err == nil {
		t.Fatal("a second version for the same halt must be refused")
	}
	if err := cmdAddUpgrade([]string{"-home", home, "-version", "v1.5.1", "-binary", fix, "-gnoroot", root, "-rolling", "-now"}); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(lay.Upgrades(), ".*"))
	if len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

func TestVersionShowsTheAttribution(t *testing.T) {
	// NOTICE.md, section 7 b): the attribution is part of the Appropriate
	// Legal Notices the program displays, and a fork must keep it.
	out := versionText()
	for _, want := range []string{"gnovisor version: ", "GnoVisor, by AviaOne.com", "AGPL-3.0", "NOTICE.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output lacks %q:\n%s", want, out)
		}
	}
}
