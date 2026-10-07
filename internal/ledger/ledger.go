// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package ledger reads a gno.land upgrade ledger, upgrades.json.
//
// The format and its rules belong to gno.land
// (gno.land/pkg/upgrades/ledger.go at commit
// 156777e0db0a0388ca6ab2e4f241d659fdde5bb7). GnoVisor imports no gno.land
// code: the file is data, so this package rereads it and re-implements the
// same checks, line for line, so that a ledger gno.land would refuse is
// refused here too.
//
// The contract (same source): a version runs the blocks from its own
// halt_height + 1 (1 for genesis) up to and including the next coordinated
// upgrade's halt_height. A rolling release has no halt and bounds no range:
// it can serve the same blocks as the entry before it.
package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AviaOne/gnovisor/internal/semver"
)

// SchemaVersion is the only format gno.land writes (ledger.go).
const SchemaVersion = 1

// imagePrefix is the image every entry must name (ledger.go, Validate).
const imagePrefix = "ghcr.io/gnolang/gno/gnoland:"

// Kind of entry.
type Kind string

const (
	KindGenesis Kind = "genesis"
	KindUpgrade Kind = "upgrade"
	KindRolling Kind = "rolling"
)

// Ledger is one chain's upgrades.json.
type Ledger struct {
	Schema        string    `json:"$schema,omitempty"`
	SchemaVersion int       `json:"schema_version"`
	ChainID       string    `json:"chain_id"`
	GenesisSHA256 string    `json:"genesis_sha256"`
	GenesisTime   time.Time `json:"genesis_time"`
	Upgrades      []Entry   `json:"upgrades"`
}

// Entry is one binary the network ran or will run.
type Entry struct {
	Kind           Kind              `json:"kind"`
	Version        string            `json:"version"`
	Commit         string            `json:"commit"`
	HaltHeight     *int64            `json:"halt_height"`
	HaltTime       *time.Time        `json:"halt_time"`
	HaltMinVersion *string           `json:"halt_min_version"`
	Proposal       *int64            `json:"proposal"`
	Image          Image             `json:"image"`
	Binaries       map[string]string `json:"binaries"`
	RanAs          *string           `json:"ran_as"`
	Release        string            `json:"release"`
}

// Image is the gnoland container image of an entry. GnoVisor takes the
// binary from it: the image is built with CGO_ENABLED=0 and its binary
// starts on any Linux, while the release binaries need glibc 2.38. Digest is
// null until gno.land's CI has built the image.
type Image struct {
	Ref    string  `json:"ref"`
	Digest *string `json:"digest"`
}

// Range is the blocks one version serves. To is 0 for the last range.
// Rolling lists the patches released into the range, in ledger order.
type Range struct {
	Version  string
	Commit   string
	From, To int64
	Rolling  []string
}

var (
	hex40     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	platform  = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	checksumQ = regexp.MustCompile(`[?&]checksum=sha256:([0-9a-f]{64})$`)
)

// Parse decodes a ledger. Unknown fields are an error, as in gno.land: a
// misspelled nullable field would otherwise read as "pending".
func Parse(data []byte) (*Ledger, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var l Ledger
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("parse upgrades.json: %w", err)
	}
	return &l, nil
}

// ParseVersion normalises a tag the way gno.land does: canonical
// vMAJOR.MINOR.PATCH with an optional pre-release, build metadata dropped.
func ParseVersion(v string) (string, bool) {
	if !semver.IsValid(v) {
		return "", false
	}
	return semver.Canonical(v), true
}

// Validate checks every invariant gno.land checks and reports all at once.
func (l *Ledger) Validate() error {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if l.SchemaVersion != SchemaVersion {
		fail("schema_version is %d, this tooling reads %d", l.SchemaVersion, SchemaVersion)
	}
	if l.ChainID == "" {
		fail("chain_id is empty")
	}
	if !hex64.MatchString(l.GenesisSHA256) {
		fail("genesis_sha256 %q is not 64 lowercase hex characters", l.GenesisSHA256)
	}
	if l.GenesisTime.IsZero() {
		fail("genesis_time is missing")
	}
	if len(l.Upgrades) == 0 {
		fail("no entries: the ledger needs at least the genesis")
		return errors.New(strings.Join(problems, "\n"))
	}

	var (
		prevVersion string
		prevHeight  int64
		prevTime    time.Time
		genesisSeen int
	)
	for i, e := range l.Upgrades {
		at := fmt.Sprintf("upgrades[%d] (%s)", i, e.Version)

		switch e.Kind {
		case KindGenesis:
			genesisSeen++
			if i != 0 {
				fail("%s: kind genesis is only valid for the first entry", at)
			}
			if e.HaltHeight != nil {
				fail("%s: genesis has no halt_height", at)
			}
		case KindUpgrade:
			if i == 0 {
				fail("%s: the first entry must be the genesis, got kind %q", at, e.Kind)
			}
			if e.HaltHeight == nil {
				fail("%s: an upgrade needs the halt_height that activated it", at)
			} else if *e.HaltHeight <= prevHeight {
				fail("%s: halt_height %d does not increase on the previous entry's %d", at, *e.HaltHeight, prevHeight)
			}
		case KindRolling:
			if i == 0 {
				fail("%s: the first entry must be the genesis, got kind %q", at, e.Kind)
			}
			if e.HaltHeight != nil {
				fail("%s: a rolling release has no halt_height", at)
			}
			if e.HaltTime != nil {
				fail("%s: a rolling release has no halt_time", at)
			}
			if e.HaltMinVersion != nil {
				fail("%s: a rolling release sets no halt_min_version", at)
			}
			if e.Proposal != nil {
				fail("%s: a rolling release has no proposal", at)
			}
			if c, ok := ParseVersion(e.Version); ok && prevVersion != "" && semver.MajorMinor(c) != semver.MajorMinor(prevVersion) {
				fail("%s: a rolling release stays on its predecessor's MINOR (%s), got %s", at, semver.MajorMinor(prevVersion), e.Version)
			}
		default:
			fail("%s: unknown kind %q", at, e.Kind)
		}
		if e.HaltHeight != nil {
			prevHeight = *e.HaltHeight
		}

		canon, ok := ParseVersion(e.Version)
		switch {
		case !ok:
			fail("%s: version %q is not a release tag the node can order", at, e.Version)
		case prevVersion != "" && semver.Compare(canon, prevVersion) <= 0:
			fail("%s: version does not increase on the previous entry's %s", at, prevVersion)
		}
		if ok {
			prevVersion = canon
		}

		if !hex40.MatchString(e.Commit) {
			fail("%s: commit %q is not a 40-character lowercase sha", at, e.Commit)
		}
		if e.HaltTime != nil {
			if !prevTime.IsZero() && !e.HaltTime.After(prevTime) {
				fail("%s: halt_time %s does not follow the previous entry's %s", at, e.HaltTime.UTC().Format(time.RFC3339), prevTime.UTC().Format(time.RFC3339))
			}
			prevTime = *e.HaltTime
		}
		if e.HaltMinVersion != nil {
			switch mv, mok := ParseVersion(*e.HaltMinVersion); {
			case *e.HaltMinVersion == "":
				fail("%s: halt_min_version is \"\"; use null when the proposal set no floor", at)
			case !mok:
				fail("%s: halt_min_version %q is not a release tag the node can parse", at, *e.HaltMinVersion)
			case ok && semver.Compare(mv, canon) > 0:
				fail("%s: halt_min_version %s is above the version that ran (%s)", at, *e.HaltMinVersion, e.Version)
			}
		}
		if e.Proposal != nil && *e.Proposal < 0 {
			fail("%s: proposal %d is negative", at, *e.Proposal)
		}
		if e.Image.Ref != imagePrefix+e.Version {
			fail("%s: image.ref %q is not %s%s", at, e.Image.Ref, imagePrefix, e.Version)
		}
		if e.Image.Digest != nil && !digestRE.MatchString(*e.Image.Digest) {
			fail("%s: image.digest %q is not sha256: followed by 64 hex characters", at, *e.Image.Digest)
		}
		if len(e.Binaries) == 0 {
			fail("%s: binaries is empty; a supervisor needs one download per platform", at)
		}
		for plat, url := range e.Binaries {
			if !platform.MatchString(plat) {
				fail("%s: binaries key %q is not a platform of the form os/arch", at, plat)
			}
			if !checksumQ.MatchString(url) {
				fail("%s: binaries[%s] has no ?checksum=sha256:<64 hex> suffix", at, plat)
			}
		}
		if e.RanAs != nil && !digestRE.MatchString(*e.RanAs) {
			fail("%s: ran_as %q is not sha256: followed by 64 hex characters", at, *e.RanAs)
		}
		if e.Release == "" {
			fail("%s: release link is empty", at)
		}
	}
	if genesisSeen != 1 {
		fail("exactly one genesis entry is expected, found %d", genesisSeen)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "\n"))
}

// BlockRanges is the contract made explicit, as in gno.land: only the
// genesis and the coordinated upgrades open a range; a rolling release is
// attached to the range it was released into.
func (l *Ledger) BlockRanges() []Range {
	ranges := make([]Range, 0, len(l.Upgrades))
	for _, e := range l.Upgrades {
		if e.Kind == KindRolling {
			if n := len(ranges); n > 0 {
				ranges[n-1].Rolling = append(ranges[n-1].Rolling, e.Version)
			}
			continue
		}
		from := int64(1)
		if e.HaltHeight != nil {
			from = *e.HaltHeight + 1
		}
		if n := len(ranges); n > 0 {
			ranges[n-1].To = from - 1
		}
		ranges = append(ranges, Range{Version: e.Version, Commit: e.Commit, From: from})
	}
	return ranges
}

// Entry returns the entry for a version.
func (l *Ledger) Entry(version string) (*Entry, bool) {
	for i := range l.Upgrades {
		if l.Upgrades[i].Version == version {
			return &l.Upgrades[i], true
		}
	}
	return nil, false
}

// UpgradeAt returns the coordinated upgrade activated by the halt at h.
func (l *Ledger) UpgradeAt(h int64) (*Entry, bool) {
	for i := range l.Upgrades {
		e := &l.Upgrades[i]
		if e.Kind == KindUpgrade && e.HaltHeight != nil && *e.HaltHeight == h {
			return e, true
		}
	}
	return nil, false
}

// RangeOf returns the range a version belongs to, rolling patches included.
func (l *Ledger) RangeOf(version string) (Range, int, bool) {
	ranges := l.BlockRanges()
	for i, r := range ranges {
		if r.Version == version {
			return r, i, true
		}
		for _, v := range r.Rolling {
			if v == version {
				return r, i, true
			}
		}
	}
	return Range{}, 0, false
}

// Newest returns the newest version a range offers: its last rolling
// patch, or the version that opened it. Any of them serves the same blocks.
func (r Range) Newest() string {
	if n := len(r.Rolling); n > 0 {
		return r.Rolling[n-1]
	}
	return r.Version
}

// Serves reports whether version serves the blocks of r.
func (r Range) Serves(version string) bool {
	if r.Version == version {
		return true
	}
	for _, v := range r.Rolling {
		if v == version {
			return true
		}
	}
	return false
}

// Binary returns the download URL and the expected sha256 of an entry for
// a platform ("linux/amd64").
func (e *Entry) Binary(plat string) (url, sha string, err error) {
	u, ok := e.Binaries[plat]
	if !ok {
		return "", "", fmt.Errorf("%s has no binary for %s", e.Version, plat)
	}
	m := checksumQ.FindStringSubmatch(u)
	if m == nil {
		return "", "", fmt.Errorf("%s: binaries[%s] has no checksum", e.Version, plat)
	}
	return u, m[1], nil
}

// FindBySHA256 returns the entry whose binary for plat has this digest.
func (l *Ledger) FindBySHA256(plat, sha string) (*Entry, bool) {
	for i := range l.Upgrades {
		e := &l.Upgrades[i]
		if _, s, err := e.Binary(plat); err == nil && s == sha {
			return e, true
		}
	}
	return nil, false
}
