// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package prepare

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
	"github.com/AviaOne/gnovisor/internal/testutil"
)

func setup(t *testing.T) (*testutil.Fixture, *ledger.Ledger, layout.Layout, Options) {
	t.Helper()
	f := testutil.NewFixture(t, "test-1",
		&testutil.Release{Kind: "genesis", Version: "v1.5.0", Binary: []byte("bin-1.5.0")},
		&testutil.Release{Kind: "upgrade", Version: "v1.6.0", HaltHeight: 100, Binary: []byte("bin-1.6.0")},
	)
	l, err := ledger.Parse(f.Ledger(-1))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	lay := layout.New(t.TempDir())
	return f, l, lay, Options{Platform: "linux/amd64", AutoDownload: true, GnorootRepo: f.Repo, HTTPClient: f.Client(), Registry: f.Server.URL}
}

func TestBinaryFromImageAndGnoroot(t *testing.T) {
	f, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	dir, err := Version(context.Background(), lay, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if dir != lay.Upgrade("v1.6.0") {
		t.Fatal(dir)
	}
	b, err := os.ReadFile(filepath.Join(layout.Gnoroot(dir), "gnovm", "stdlibs", "VERSION"))
	if err != nil || strings.TrimSpace(string(b)) != "v1.6.0" {
		t.Fatalf("GNOROOT at the wrong tag: %q %v", b, err)
	}
	st, err := os.Stat(layout.Binary(dir))
	if err != nil || st.Mode()&0o100 == 0 {
		t.Fatal("binary not executable")
	}
	// The binary is the image's, not the release download.
	if b, _ := os.ReadFile(layout.Binary(dir)); string(b) != "bin-1.6.0" {
		t.Fatalf("binary = %q", b)
	}
	if f.Hits["/releases/download/v1.6.0/gnoland_linux_amd64"] != 0 {
		t.Fatal("the release binary must not be downloaded")
	}
	i, err := layout.ReadInfo(dir)
	if err != nil || i.Image != f.Releases[1].ImageDigest || i.SHA256 != testutil.SHA256([]byte("bin-1.6.0")) {
		t.Fatalf("version.json = %+v %v", i, err)
	}
	// Prepared once: a second call verifies and returns.
	if _, err := Version(context.Background(), lay, e, o); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(lay.Upgrades(), ".*"))
	if len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

func TestTamperedLayerIsRefusedAndNothingIsLeft(t *testing.T) {
	f, l, lay, o := setup(t)
	f.Serve("v1.6.0", []byte("tampered"))
	e, _ := l.Entry("v1.6.0")
	_, err := Version(context.Background(), lay, e, o)
	if err == nil || !strings.Contains(err.Error(), "content has digest") {
		t.Fatalf("err = %v", err)
	}
	if lay.VersionDir("v1.6.0") != "" {
		t.Fatal("a version must not look ready after a failed download")
	}
	if _, err := os.Stat(lay.Upgrade("v1.6.0")); !os.IsNotExist(err) {
		t.Fatal("nothing must be left in place of the version")
	}
}

func TestDepositedBinaryHasPriority(t *testing.T) {
	f, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	dep := layout.Binary(lay.Upgrade("v1.6.0"))
	if err := os.MkdirAll(filepath.Dir(dep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dep, []byte("bin-1.6.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.AutoDownload = false
	if _, err := Version(context.Background(), lay, e, o); err != nil {
		t.Fatal(err)
	}
	if f.ImageHits() != 0 {
		t.Fatal("a deposited binary must not be downloaded again")
	}

	// A deposited binary with another digest is refused.
	lay2 := layout.New(t.TempDir())
	dep = layout.Binary(lay2.Upgrade("v1.6.0"))
	_ = os.MkdirAll(filepath.Dir(dep), 0o755)
	_ = os.WriteFile(dep, []byte("other"), 0o755)
	if _, err := Version(context.Background(), lay2, e, o); err == nil {
		t.Fatal("a deposited binary with the wrong digest must be refused")
	}
}

func TestNoDownloadWhenDisabled(t *testing.T) {
	_, l, lay, o := setup(t)
	o.AutoDownload = false
	e, _ := l.Entry("v1.6.0")
	if _, err := Version(context.Background(), lay, e, o); err == nil || !strings.Contains(err.Error(), "auto_download") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommitMismatchIsRefused(t *testing.T) {
	_, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	e.Commit = strings.Repeat("0", 40)
	_, err := Version(context.Background(), lay, e, o)
	if err == nil || !strings.Contains(err.Error(), "the ledger says") {
		t.Fatalf("err = %v", err)
	}
	if lay.VersionDir("v1.6.0") != "" {
		t.Fatal("a version must not look ready when its GNOROOT is refused")
	}
}

func TestHTTPRegistryIsRefused(t *testing.T) {
	_, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	o.Registry = strings.Replace(o.Registry, "https://", "http://", 1)
	if _, err := Version(context.Background(), lay, e, o); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingDigestReadsTheImageByItsTag(t *testing.T) {
	// The ledger lists the version before gno.land filled the digest: the
	// image is read by its tag instead of leaving a halted node waiting.
	f, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	e.Image.Digest = nil
	dir, err := Version(context.Background(), lay, e, o)
	if err != nil {
		t.Fatal(err)
	}
	i, _ := layout.ReadInfo(dir)
	if i.Image != f.Releases[1].ImageDigest {
		t.Fatalf("digest read %s, want %s", i.Image, f.Releases[1].ImageDigest)
	}
	// Once the ledger gives the same digest, the prepared version stands.
	right := f.Releases[1].ImageDigest
	e.Image.Digest = &right
	if _, err := Version(context.Background(), lay, e, o); err != nil {
		t.Fatal(err)
	}
}

func TestAnotherDigestInTheLedgerPreparesTheVersionAgain(t *testing.T) {
	f, l, lay, o := setup(t)
	e, _ := l.Entry("v1.6.0")
	other := f.Releases[0].ImageDigest // the image of v1.5.0, bin-1.5.0
	e.Image.Digest = &other
	dir, err := Version(context.Background(), lay, e, o)
	if err != nil {
		t.Fatal(err)
	}
	// gno.land now names another image for v1.6.0: prepared again from it.
	right := f.Releases[1].ImageDigest
	e.Image.Digest = &right
	if _, err := Version(context.Background(), lay, e, o); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(layout.Binary(dir)); string(b) != "bin-1.6.0" {
		t.Fatalf("binary = %q, want the one of the image the ledger names now", b)
	}
	if i, _ := layout.ReadInfo(dir); i.Image != right {
		t.Fatalf("image = %s", i.Image)
	}
}

func TestArm64ManifestIsPicked(t *testing.T) {
	_, l, lay, o := setup(t)
	o.Platform = "linux/arm64"
	e, _ := l.Entry("v1.6.0")
	if _, err := Version(context.Background(), lay, e, o); err != nil {
		t.Fatal(err)
	}
	o.Platform = "linux/riscv64"
	lay2 := layout.New(t.TempDir())
	if _, err := Version(context.Background(), lay2, e, o); err == nil {
		t.Fatal("a platform with no binary must be refused")
	}
}

func TestIntoFillsANewDirectory(t *testing.T) {
	f, l, _, o := setup(t)
	e, _ := l.Entry("v1.5.0")
	dir := filepath.Join(t.TempDir(), "genesis")
	i, err := Into(context.Background(), dir, e, o)
	if err != nil {
		t.Fatal(err)
	}
	if i.Version != "v1.5.0" || i.Image != f.Releases[0].ImageDigest {
		t.Fatalf("info = %+v", i)
	}
	if _, err := layout.Verify(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Into(context.Background(), dir, e, o); err == nil {
		t.Fatal("an existing directory must be refused")
	}
}
