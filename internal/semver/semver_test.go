// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package semver

import "testing"

func TestCompareOrdersReleaseCandidatesBelowTheirRelease(t *testing.T) {
	// Ascending order; RELEASING.md: a pre-release sorts below the release it
	// leads to. Rest from SemVer 2.0.0 item 11.
	order := []string{
		"v1.0.0-alpha",
		"v1.0.0-alpha.1",
		"v1.0.0-alpha.beta",
		"v1.0.0-beta",
		"v1.0.0-beta.2",
		"v1.0.0-beta.11",
		"v1.0.0-rc.1",
		"v1.0.0",
		"v1.2.0",
		"v1.5.0",
		"v1.6.0-rc.1",
		"v1.6.0-rc.2",
		"v1.6.0",
		"v1.6.1",
		"v1.10.0",
		"v2.0.0",
	}
	for i := range order {
		for j := range order {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(order[i], order[j]); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

func TestParseRefusesNonCanonicalTags(t *testing.T) {
	for _, v := range []string{
		"", "1.2.0", "v1.2", "v1", "v01.2.0", "v1.02.0", "v1.2.00",
		"v1.2.0-", "v1.2.0-rc..1", "v1.2.0-01", "v1.2.0+", "v1.2.0-rc_1",
		"chain/mainnet", "heads/chain/mainnet.3444+e75fef82c", "develop",
	} {
		if IsValid(v) {
			t.Errorf("IsValid(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"v0.0.0", "v1.2.0", "v1.6.0-rc.1", "v1.2.0+build.5", "v1.2.0-0a"} {
		if !IsValid(v) {
			t.Errorf("IsValid(%q) = false, want true", v)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := Canonical("v1.2.0-rc.1+meta"); got != "v1.2.0-rc.1" {
		t.Errorf("Canonical = %q", got)
	}
	if got := MajorMinor("v1.6.3-rc.1"); got != "v1.6" {
		t.Errorf("MajorMinor = %q", got)
	}
	if got := Final("v1.6.0-rc.2"); got != "v1.6.0" {
		t.Errorf("Final = %q", got)
	}
	if Compare("bad", "v0.0.1") != -1 || Compare("v0.0.1", "bad") != 1 || Compare("x", "y") != 0 {
		t.Error("invalid versions must sort below valid ones")
	}
}
