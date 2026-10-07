// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package semver orders gno.land release tags.
//
// gno.land orders its tags with golang.org/x/mod/semver
// (gno.land/pkg/upgrades/ledger.go, ParseVersion). GnoVisor imports nothing
// outside the standard library, so this package reimplements the subset the
// ledger rules need: canonical vMAJOR.MINOR.PATCH with an optional
// pre-release and build metadata, compared by Semantic Versioning 2.0.0
// precedence.
package semver

import "strings"

// Version is a parsed tag.
type Version struct {
	Major, Minor, Patch string
	Prerelease          string // without the leading "-"
	Build               string // without the leading "+"
}

// Parse accepts only the canonical form vMAJOR.MINOR.PATCH[-pre][+build]:
// the ledger refuses any other shape (ledger.go, ParseVersion requires
// semver.Canonical(v) == v once build metadata is dropped).
func Parse(v string) (Version, bool) {
	var out Version
	if !strings.HasPrefix(v, "v") {
		return out, false
	}
	rest := v[1:]
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		out.Build = rest[i+1:]
		rest = rest[:i]
		if !validIdents(out.Build, false) {
			return Version{}, false
		}
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		out.Prerelease = rest[i+1:]
		rest = rest[:i]
		if !validIdents(out.Prerelease, true) {
			return Version{}, false
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	for _, p := range parts {
		if !isNum(p) {
			return Version{}, false
		}
	}
	out.Major, out.Minor, out.Patch = parts[0], parts[1], parts[2]
	return out, true
}

// IsValid reports whether v is a canonical tag.
func IsValid(v string) bool {
	_, ok := Parse(v)
	return ok
}

// Canonical returns v without build metadata, or "" if v is not valid.
func Canonical(v string) string {
	p, ok := Parse(v)
	if !ok {
		return ""
	}
	s := "v" + p.Major + "." + p.Minor + "." + p.Patch
	if p.Prerelease != "" {
		s += "-" + p.Prerelease
	}
	return s
}

// MajorMinor returns vMAJOR.MINOR, or "" if v is not valid.
func MajorMinor(v string) string {
	p, ok := Parse(v)
	if !ok {
		return ""
	}
	return "v" + p.Major + "." + p.Minor
}

// Final returns the release a pre-release rehearses: v1.6.0-rc.2 gives v1.6.0.
func Final(v string) string {
	p, ok := Parse(v)
	if !ok {
		return v
	}
	return "v" + p.Major + "." + p.Minor + "." + p.Patch
}

// Compare returns -1, 0 or +1. An invalid version sorts below every valid
// one, and two invalid versions are equal, as in golang.org/x/mod/semver.
func Compare(a, b string) int {
	pa, oka := Parse(a)
	pb, okb := Parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	if c := cmpNum(pa.Major, pb.Major); c != 0 {
		return c
	}
	if c := cmpNum(pa.Minor, pb.Minor); c != 0 {
		return c
	}
	if c := cmpNum(pa.Patch, pb.Patch); c != 0 {
		return c
	}
	return cmpPre(pa.Prerelease, pb.Prerelease)
}

// isNum is a decimal without a leading zero (SemVer 2.0.0, item 2).
func isNum(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s == "0" || s[0] != '0'
}

func validIdents(s string, pre bool) bool {
	if s == "" {
		return false
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		allDigits := true
		for i := 0; i < len(id); i++ {
			c := id[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
				allDigits = false
			default:
				return false
			}
		}
		// A numeric pre-release identifier has no leading zero (item 9).
		if pre && allDigits && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

func cmpNum(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// cmpPre follows SemVer 2.0.0 item 11: no pre-release ranks above any
// pre-release; identifiers compare one by one, numeric below alphanumeric.
func cmpPre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	ia, ib := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ia) && i < len(ib); i++ {
		x, y := ia[i], ib[i]
		if x == y {
			continue
		}
		nx, ny := isDigits(x), isDigits(y)
		switch {
		case nx && ny:
			return cmpNum(x, y)
		case nx:
			return -1
		case ny:
			return 1
		default:
			return strings.Compare(x, y)
		}
	}
	switch {
	case len(ia) < len(ib):
		return -1
	case len(ia) > len(ib):
		return 1
	}
	return 0
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
