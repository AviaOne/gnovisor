// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package local

import (
	"os"
	"testing"
	"time"

	"github.com/AviaOne/gnovisor/internal/layout"
)

func TestReadWriteAndLookups(t *testing.T) {
	l := layout.New(t.TempDir())
	if err := os.MkdirAll(l.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := Read(l)
	if err != nil || len(f.Upgrades) != 0 {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	f.Upgrades = append(f.Upgrades,
		Entry{Version: "v1.6.0", HaltHeight: 200, SHA256: "aa", Added: time.Now().UTC()},
		Entry{Version: "v1.6.1", SHA256: "bb", Now: true, Added: time.Now().UTC()},
	)
	if err := Write(l, f); err != nil {
		t.Fatal(err)
	}
	g, err := Read(l)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := g.AtHalt(200); !ok || e.Version != "v1.6.0" {
		t.Fatal("AtHalt")
	}
	if _, ok := g.AtHalt(0); ok {
		t.Fatal("height 0 is no halt")
	}
	if r := g.Rolling(); len(r) != 1 || r[0].Version != "v1.6.1" || !r[0].Now {
		t.Fatalf("Rolling = %+v", r)
	}
	if !g.Has("v1.6.1") || g.Has("v9.9.9") {
		t.Fatal("Has")
	}
}

func TestBrokenFileIsAnError(t *testing.T) {
	l := layout.New(t.TempDir())
	_ = os.MkdirAll(l.Root, 0o755)
	_ = os.WriteFile(Path(l), []byte("{"), 0o644)
	if _, err := Read(l); err == nil {
		t.Fatal("a broken file must not read as empty")
	}
}
