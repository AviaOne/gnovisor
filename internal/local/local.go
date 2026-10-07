// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package local holds the versions an operator added by hand, outside the
// gno.land ledger (decision D9).
//
// The case it serves is rare: a security fix handed to validators privately,
// before the flaw is disclosed. The public ledger cannot list such a binary,
// so without this file a node halted for it would stay stopped (D4). The
// operator gives the binary and its GNOROOT with `gnovisor add-upgrade`, and
// takes responsibility for them: nothing published can confirm them.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AviaOne/gnovisor/internal/layout"
)

// FileName is the file, inside <node dir>/gnovisor/.
const FileName = "local-upgrades.json"

// Entry is one version added by hand.
type Entry struct {
	Version string `json:"version"`
	// HaltHeight is the coordinated halt this version runs after; 0 for a
	// rolling version, installed without a halt.
	HaltHeight int64 `json:"halt_height"`
	// Now installs a rolling version at the next poll instead of waiting
	// for rolling_window.
	Now    bool      `json:"now"`
	SHA256 string    `json:"sha256"`
	Added  time.Time `json:"added"`
}

// File is local-upgrades.json.
type File struct {
	Upgrades []Entry `json:"upgrades"`
}

// Path of the file for a layout.
func Path(l layout.Layout) string { return filepath.Join(l.Root, FileName) }

// Read reads the file; a missing file is empty.
func Read(l layout.Layout) (File, error) {
	var f File
	b, err := os.ReadFile(Path(l))
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s: %w", Path(l), err)
	}
	return f, nil
}

// Write writes the file atomically.
func Write(l layout.Layout, f File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return layout.WriteFileAtomic(Path(l), append(b, '\n'), 0o644)
}

// AtHalt returns the entry added for the halt at h.
func (f File) AtHalt(h int64) (Entry, bool) {
	for _, e := range f.Upgrades {
		if e.HaltHeight == h && h > 0 {
			return e, true
		}
	}
	return Entry{}, false
}

// Rolling returns the rolling entries, in the order they were added.
func (f File) Rolling() []Entry {
	var out []Entry
	for _, e := range f.Upgrades {
		if e.HaltHeight == 0 {
			out = append(out, e)
		}
	}
	return out
}

// Has reports whether a version is already listed.
func (f File) Has(version string) bool {
	for _, e := range f.Upgrades {
		if e.Version == version {
			return true
		}
	}
	return false
}
