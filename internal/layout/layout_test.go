// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package layout

import (
	"os"
	"path/filepath"
	"testing"
)

func makeVersion(t *testing.T, dir, version, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Gnoroot(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Binary(dir), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256File(Binary(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteInfo(dir, Info{Version: version, Commit: "c", SHA256: sum}); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchIsRelativeAndReplaces(t *testing.T) {
	l := New(t.TempDir())
	makeVersion(t, l.Genesis(), "v1.5.0", "a")
	makeVersion(t, l.Upgrade("v1.6.0"), "v1.6.0", "b")
	if err := l.Switch(l.Genesis()); err != nil {
		t.Fatal(err)
	}
	if err := l.Switch(l.Upgrade("v1.6.0")); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(l.Current())
	if err != nil || target != filepath.Join("upgrades", "v1.6.0") {
		t.Fatalf("link = %q %v; a relative link survives moving the node directory", target, err)
	}
	dir, err := l.CurrentDir()
	if err != nil || dir != l.Upgrade("v1.6.0") {
		t.Fatalf("CurrentDir = %q %v", dir, err)
	}
	if _, err := os.Lstat(l.Current() + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary link left behind")
	}
	if l.VersionDir("v1.5.0") != l.Genesis() || l.VersionDir("v1.6.0") != l.Upgrade("v1.6.0") || l.VersionDir("v9.0.0") != "" {
		t.Fatal("VersionDir")
	}
}

func TestVerifyDetectsAChangedBinary(t *testing.T) {
	l := New(t.TempDir())
	makeVersion(t, l.Genesis(), "v1.5.0", "a")
	if _, err := Verify(l.Genesis()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Binary(l.Genesis()), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(l.Genesis()); err == nil {
		t.Fatal("a binary that no longer matches version.json must be refused")
	}
}

func TestStateRoundTrip(t *testing.T) {
	l := New(t.TempDir())
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := l.ReadState()
	if err != nil || s.LastHalt != 0 || s.Pending != nil {
		t.Fatalf("missing state: %+v %v", s, err)
	}
	want := State{LastHalt: 162200, Pending: &Pending{Height: 200000}}
	if err := l.WriteState(want); err != nil {
		t.Fatal(err)
	}
	got, err := l.ReadState()
	if err != nil || got.LastHalt != 162200 || got.Pending == nil || got.Pending.Height != 200000 {
		t.Fatalf("%+v %v", got, err)
	}
	entries, _ := os.ReadDir(l.Root)
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}
