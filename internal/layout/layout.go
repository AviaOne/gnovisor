// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package layout owns the files under <node dir>/gnovisor/.
//
//	gnovisor/
//	    gnovisor.toml
//	    genesis/{bin/gnoland, gnoroot/, version.json}
//	    upgrades/<vX.Y.Z>/{bin/gnoland, gnoroot/, version.json}
//	    current -> genesis | upgrades/<vX.Y.Z>
//	    backups/
//	    state.json
//
// The tree follows Cosmovisor's (cosmovisor/genesis, cosmovisor/upgrades,
// cosmovisor/current) so validators find their way, with one change: a
// version directory is named by its tag, because the gno.land ledger orders
// versions by tag and has no upgrade plan names.
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Layout is the gnovisor/ directory of one node.
type Layout struct {
	Home string // the node directory, --data-dir of gnoland
	Root string // <Home>/gnovisor
}

// New returns the layout of the node directory home.
func New(home string) Layout {
	return Layout{Home: home, Root: filepath.Join(home, "gnovisor")}
}

func (l Layout) Genesis() string         { return filepath.Join(l.Root, "genesis") }
func (l Layout) Upgrades() string        { return filepath.Join(l.Root, "upgrades") }
func (l Layout) Upgrade(v string) string { return filepath.Join(l.Upgrades(), v) }
func (l Layout) Current() string         { return filepath.Join(l.Root, "current") }
func (l Layout) StateFile() string       { return filepath.Join(l.Root, "state.json") }

// Binary and Gnoroot of a version directory.
func Binary(dir string) string   { return filepath.Join(dir, "bin", "gnoland") }
func Gnoroot(dir string) string  { return filepath.Join(dir, "gnoroot") }
func infoFile(dir string) string { return filepath.Join(dir, "version.json") }

// Info is version.json: what a version directory holds, so the running
// version is never guessed from what the node reports (source of truth,
// 3.2: a node's reported version does not identify its binary).
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	SHA256  string `json:"sha256"`
	// Image is the digest of the ledger's image the binary was taken from,
	// empty for a binary that is the ledger's release download.
	Image string `json:"image,omitempty"`
}

// ReadInfo reads dir/version.json.
func ReadInfo(dir string) (Info, error) {
	var i Info
	b, err := os.ReadFile(infoFile(dir))
	if err != nil {
		return i, err
	}
	if err := json.Unmarshal(b, &i); err != nil {
		return i, fmt.Errorf("%s: %w", infoFile(dir), err)
	}
	if i.Version == "" || i.SHA256 == "" {
		return i, fmt.Errorf("%s: version or sha256 missing", infoFile(dir))
	}
	return i, nil
}

// WriteInfo writes dir/version.json.
func WriteInfo(dir string, i Info) error {
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(infoFile(dir), append(b, '\n'), 0o644)
}

// SHA256File hashes a file.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify checks that dir's binary still has the digest version.json
// recorded.
func Verify(dir string) (Info, error) {
	i, err := ReadInfo(dir)
	if err != nil {
		return i, err
	}
	got, err := SHA256File(Binary(dir))
	if err != nil {
		return i, err
	}
	if got != i.SHA256 {
		return i, fmt.Errorf("%s: sha256 %s, version.json records %s", Binary(dir), got, i.SHA256)
	}
	if st, err := os.Stat(Gnoroot(dir)); err != nil || !st.IsDir() {
		return i, fmt.Errorf("%s: GNOROOT missing", Gnoroot(dir))
	}
	return i, nil
}

// CurrentDir resolves the current link to an absolute directory.
func (l Layout) CurrentDir() (string, error) {
	target, err := os.Readlink(l.Current())
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(l.Root, target)
	}
	return target, nil
}

// VersionDir returns the directory of a prepared version, or "" if none.
func (l Layout) VersionDir(v string) string {
	if i, err := ReadInfo(l.Genesis()); err == nil && i.Version == v {
		return l.Genesis()
	}
	if i, err := ReadInfo(l.Upgrade(v)); err == nil && i.Version == v {
		return l.Upgrade(v)
	}
	return ""
}

// Switch points current at dir, atomically: a new link is created beside
// it and renamed over it, so current never stops existing.
func (l Layout) Switch(dir string) error {
	rel, err := filepath.Rel(l.Root, dir)
	if err != nil {
		return err
	}
	tmp := l.Current() + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(rel, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.Current()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(l.Root)
}

// State is what GnoVisor has done, kept across its own restarts.
type State struct {
	// LastHalt is the last halt height whose switch was completed.
	LastHalt int64 `json:"last_halt"`
	// Pending is a halt reached whose version was not ready: the node must
	// not be started on the old binary until it is (decision D4).
	Pending *Pending `json:"pending,omitempty"`
}

// Pending halt.
type Pending struct {
	Height int64 `json:"height"`
}

// ReadState reads state.json; a missing file is the zero state.
func (l Layout) ReadState() (State, error) {
	var s State
	b, err := os.ReadFile(l.StateFile())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", l.StateFile(), err)
	}
	return s, nil
}

// WriteState writes state.json atomically.
func (l Layout) WriteState(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(l.StateFile(), append(b, '\n'), 0o644)
}

// WriteFileAtomic writes a file through a temporary file, fsync and rename,
// so a crash leaves either the old or the new content.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
