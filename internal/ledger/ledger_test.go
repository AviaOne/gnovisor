// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// referenceLedger is the reference ledger of gno.land's own tests
// (gno.land/pkg/upgrades/ledger_test.go, validLedger, commit 156777e0).
// Keeping it identical keeps our rules aligned with theirs.
const referenceLedger = `{
  "$schema": "../upgrades/upgrades.schema.json",
  "schema_version": 1,
  "chain_id": "gnoland-1",
  "genesis_sha256": "ea22691003130eae3ba975b7d16460706b5d75ce6c04ae82c0c4faeab7de91f0",
  "genesis_time": "2026-09-12T15:00:00Z",
  "upgrades": [
    {
      "kind": "genesis",
      "version": "v1.2.0",
      "commit": "9c8eb132e483d6fd324d92c193e629ad65a98a37",
      "halt_height": null,
      "halt_time": null,
      "halt_min_version": null,
      "proposal": null,
      "image": {"ref": "ghcr.io/gnolang/gno/gnoland:v1.2.0", "digest": "sha256:4b161a2b5d5fcceab4badd4d519ea73465c078a17e6fb1e9b0bae2e01c1a7b87"},
      "binaries": {
        "linux/amd64": "https://github.com/gnolang/gno/releases/download/v1.2.0/gnoland_linux_amd64?checksum=sha256:1111111111111111111111111111111111111111111111111111111111111111"
      },
      "ran_as": null,
      "release": "https://github.com/gnolang/gno/releases/tag/v1.2.0"
    },
    {
      "kind": "upgrade",
      "version": "v1.3.0",
      "commit": "31b6650a100d9baf14e7669f8f0df924f1f841e0",
      "halt_height": 36300,
      "halt_time": "2026-09-14T09:17:06Z",
      "halt_min_version": null,
      "proposal": 0,
      "image": {"ref": "ghcr.io/gnolang/gno/gnoland:v1.3.0", "digest": null},
      "binaries": {
        "linux/amd64": "https://github.com/gnolang/gno/releases/download/v1.3.0/gnoland_linux_amd64?checksum=sha256:2222222222222222222222222222222222222222222222222222222222222222"
      },
      "ran_as": "sha256:7fffc5ac2d21d6608bed35b31cfc3f2aaefed0d6bb3cdba1d5edbfd905127a3f",
      "release": "https://github.com/gnolang/gno/releases/tag/v1.3.0"
    },
    {
      "kind": "upgrade",
      "version": "v1.4.0-rc.1",
      "commit": "00417a1be97b9a311d9669ae7aa9585b277ee594",
      "halt_height": 113000,
      "halt_time": null,
      "halt_min_version": "v1.4.0-rc.1",
      "proposal": null,
      "image": {"ref": "ghcr.io/gnolang/gno/gnoland:v1.4.0-rc.1", "digest": null},
      "binaries": {
        "linux/amd64": "https://github.com/gnolang/gno/releases/download/v1.4.0-rc.1/gnoland_linux_amd64?checksum=sha256:3333333333333333333333333333333333333333333333333333333333333333"
      },
      "ran_as": null,
      "release": "https://github.com/gnolang/gno/releases/tag/v1.4.0-rc.1"
    },
    {
      "kind": "rolling",
      "version": "v1.4.1",
      "commit": "1111111111111111111111111111111111111111",
      "halt_height": null,
      "halt_time": null,
      "halt_min_version": null,
      "proposal": null,
      "image": {"ref": "ghcr.io/gnolang/gno/gnoland:v1.4.1", "digest": null},
      "binaries": {
        "linux/amd64": "https://github.com/gnolang/gno/releases/download/v1.4.1/gnoland_linux_amd64?checksum=sha256:4444444444444444444444444444444444444444444444444444444444444444"
      },
      "ran_as": null,
      "release": "https://github.com/gnolang/gno/releases/tag/v1.4.1"
    }
  ]
}`

func parseRef(t *testing.T) *Ledger {
	t.Helper()
	l, err := Parse([]byte(referenceLedger))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestReferenceLedgerIsValid(t *testing.T) {
	if err := parseRef(t).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRealLedgersAreValid(t *testing.T) {
	for _, name := range []string{"mainnet.upgrades.json", "onyx.upgrades.json"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		l, err := Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := l.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestMainnetRangesAndLookups(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "mainnet.upgrades.json"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	r := l.BlockRanges()
	got := [][3]any{}
	for _, x := range r {
		got = append(got, [3]any{x.Version, x.From, x.To})
	}
	want := [][3]any{
		{"v1.2.0", int64(1), int64(36300)},
		{"v1.3.0", int64(36301), int64(113000)},
		{"v1.4.0", int64(113001), int64(162200)},
		{"v1.5.0", int64(162201), int64(0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges = %v, want %v", got, want)
	}
	if e, ok := l.UpgradeAt(113000); !ok || e.Version != "v1.4.0" {
		t.Fatalf("UpgradeAt(113000) = %v %v", e, ok)
	}
	if _, ok := l.UpgradeAt(113001); ok {
		t.Fatal("UpgradeAt must match the halt height exactly")
	}
	url, sha, err := l.Upgrades[3].Binary("linux/amd64")
	if err != nil || sha != "8dcff48228a881e398d238e3e14760c175c872fb164e85f21e5b4ee94a8b076d" ||
		!strings.HasPrefix(url, "https://github.com/gnolang/gno/releases/download/v1.5.0/gnoland_linux_amd64") {
		t.Fatalf("Binary = %q %q %v", url, sha, err)
	}
	if e, ok := l.FindBySHA256("linux/arm64", "22fb2d122bf4c7fae7464e9f6b5e3f2d4d03176cb5a7cc693bfd617ed8fce7c9"); !ok || e.Version != "v1.4.0" {
		t.Fatalf("FindBySHA256 = %v %v", e, ok)
	}
}

func TestRollingPatchServesItsRange(t *testing.T) {
	l := parseRef(t)
	r, i, ok := l.RangeOf("v1.4.1")
	if !ok || i != 2 || r.Version != "v1.4.0-rc.1" || r.Newest() != "v1.4.1" {
		t.Fatalf("RangeOf(v1.4.1) = %+v %d %v", r, i, ok)
	}
	if !r.Serves("v1.4.0-rc.1") || !r.Serves("v1.4.1") || r.Serves("v1.3.0") {
		t.Fatal("Serves is wrong")
	}
	if !slices.Equal(l.BlockRanges()[1].Rolling, nil) {
		t.Fatal("a rolling patch belongs only to the range it was released into")
	}
}

// One mutation per rule, the same list as gno.land's
// TestValidate_refusesEachBrokenInvariant.
func TestValidateRefusesEachBrokenInvariant(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(l *Ledger)
		want   string
	}{
		{"unknown schema version", func(l *Ledger) { l.SchemaVersion = 2 }, "schema_version"},
		{"empty chain id", func(l *Ledger) { l.ChainID = "" }, "chain_id"},
		{"genesis sha not 64 hex", func(l *Ledger) { l.GenesisSHA256 = "abc" }, "genesis_sha256"},
		{"no entries", func(l *Ledger) { l.Upgrades = nil }, "no entries"},
		{"first entry is not genesis", func(l *Ledger) { l.Upgrades[0].Kind = KindUpgrade; h := int64(1); l.Upgrades[0].HaltHeight = &h }, "first entry"},
		{"two genesis entries", func(l *Ledger) { l.Upgrades[1].Kind = KindGenesis; l.Upgrades[1].HaltHeight = nil }, "genesis"},
		{"unknown kind", func(l *Ledger) { l.Upgrades[1].Kind = "hotfix" }, "kind"},
		{"genesis with a halt height", func(l *Ledger) { h := int64(5); l.Upgrades[0].HaltHeight = &h }, "halt_height"},
		{"upgrade without a halt height", func(l *Ledger) { l.Upgrades[1].HaltHeight = nil }, "halt_height"},
		{"halt heights not increasing", func(l *Ledger) { h := int64(36300); l.Upgrades[2].HaltHeight = &h }, "halt_height"},
		{"versions not increasing", func(l *Ledger) { l.Upgrades[2].Version = "v1.2.5" }, "version"},
		{"duplicate version", func(l *Ledger) { l.Upgrades[2].Version = "v1.3.0" }, "version"},
		{"version not a release tag", func(l *Ledger) { l.Upgrades[1].Version = "chain/mainnet" }, "version"},
		{"commit not 40 hex", func(l *Ledger) { l.Upgrades[1].Commit = "31b6650a1" }, "commit"},
		{"digest without prefix", func(l *Ledger) {
			d := "7fffc5ac2d21d6608bed35b31cfc3f2aaefed0d6bb3cdba1d5edbfd905127a3f"
			l.Upgrades[1].Image.Digest = &d
		}, "digest"},
		{"empty halt_min_version is ambiguous", func(l *Ledger) { s := ""; l.Upgrades[1].HaltMinVersion = &s }, "halt_min_version"},
		{"halt_min_version not a release tag", func(l *Ledger) { s := "latest"; l.Upgrades[1].HaltMinVersion = &s }, "halt_min_version"},
		{"halt_min_version above the version that ran", func(l *Ledger) { s := "v9.0.0"; l.Upgrades[1].HaltMinVersion = &s }, "halt_min_version"},
		{"halt times not increasing", func(l *Ledger) { t2 := l.Upgrades[1].HaltTime.Add(-1); l.Upgrades[2].HaltTime = &t2 }, "halt_time"},
		{"negative proposal", func(l *Ledger) { p := int64(-1); l.Upgrades[1].Proposal = &p }, "proposal"},
		{"image ref for another version", func(l *Ledger) { l.Upgrades[1].Image.Ref = "ghcr.io/gnolang/gno/gnoland:v1.9.0" }, "image.ref"},
		{"no binaries", func(l *Ledger) { l.Upgrades[1].Binaries = nil }, "binaries"},
		{"binary without checksum", func(l *Ledger) { l.Upgrades[1].Binaries["linux/amd64"] = "https://example.com/gnoland" }, "checksum"},
		{"binary platform not os/arch", func(l *Ledger) { l.Upgrades[1].Binaries["linux"] = l.Upgrades[1].Binaries["linux/amd64"] }, "platform"},
		{"ran_as malformed", func(l *Ledger) { s := "sha-31b6650"; l.Upgrades[1].RanAs = &s }, "ran_as"},
		{"missing release link", func(l *Ledger) { l.Upgrades[1].Release = "" }, "release"},
		{"rolling with a halt height", func(l *Ledger) { h := int64(200000); l.Upgrades[3].HaltHeight = &h }, "rolling"},
		{"rolling with a halt time", func(l *Ledger) { t2 := l.Upgrades[1].HaltTime.Add(1); l.Upgrades[3].HaltTime = &t2 }, "rolling"},
		{"rolling with a version floor", func(l *Ledger) { s := "v1.4.1"; l.Upgrades[3].HaltMinVersion = &s }, "rolling"},
		{"rolling with a proposal", func(l *Ledger) { p := int64(9); l.Upgrades[3].Proposal = &p }, "rolling"},
		{"rolling as the first entry", func(l *Ledger) { l.Upgrades[0].Kind = KindRolling }, "first entry"},
		{"rolling across a MINOR", func(l *Ledger) {
			l.Upgrades[3].Version = "v1.5.0"
			l.Upgrades[3].Image.Ref = "ghcr.io/gnolang/gno/gnoland:v1.5.0"
		}, "rolling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := parseRef(t)
			tc.mutate(l)
			err := l.Validate()
			if err == nil {
				t.Fatalf("mutation %q was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseRefusesUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(strings.Replace(referenceLedger, `"chain_id"`, `"chain_idd"`, 1))); err == nil {
		t.Fatal("a typo in a field name must not silently drop the field")
	}
}

// The schema gno.land ships must describe the fields we read.
func TestSchemaDescribesTheStructs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "upgrades.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Defs       struct {
			Entry struct {
				Properties map[string]any `json:"properties"`
			} `json:"entry"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	check := func(typ reflect.Type, props map[string]any) {
		var tags []string
		for i := 0; i < typ.NumField(); i++ {
			tags = append(tags, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		var keys []string
		for k := range props {
			keys = append(keys, k)
		}
		slices.Sort(tags)
		slices.Sort(keys)
		if !slices.Equal(tags, keys) {
			t.Errorf("%s fields %v, schema %v", typ.Name(), tags, keys)
		}
	}
	check(reflect.TypeOf(Ledger{}), schema.Properties)
	check(reflect.TypeOf(Entry{}), schema.Defs.Entry.Properties)
}
