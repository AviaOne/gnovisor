// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimal = `
node_dir = "/srv/gno"  # the node
node_args = ["--chainid", "gnoland-1", "--genesis", "/srv/genesis.json", "--skip-genesis-sig-verification"]
chain_id = "gnoland-1"
rolling_window = "02:00-04:00"
`

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.RPC != "http://127.0.0.1:26657" || c.PollInterval != 5*time.Second || !c.AutoDownload || !c.Backup ||
		c.ShutdownGrace != 2*time.Minute || c.HaltConfirm != 30*time.Second || c.RestartTimeout != 10*time.Minute ||
		c.GnorootRepo != DefaultGnorootRepo {
		t.Fatalf("defaults: %+v", c)
	}
	if c.LedgerURL != "https://raw.githubusercontent.com/gnolang/gno/refs/heads/chain/mainnet/misc/deployments/mainnet.gno.land/upgrades.json" {
		t.Fatalf("ledger_url = %s", c.LedgerURL)
	}
	if !slices.Equal(c.NodeArgs, []string{"--chainid", "gnoland-1", "--genesis", "/srv/genesis.json", "--skip-genesis-sig-verification"}) {
		t.Fatalf("node_args = %q", c.NodeArgs)
	}
}

func TestParseRefuses(t *testing.T) {
	cases := map[string]struct{ text, want string }{
		"no window":        {strings.Replace(minimal, `rolling_window = "02:00-04:00"`, "", 1), "rolling_window: required"},
		"bad window":       {strings.Replace(minimal, "02:00-04:00", "2h", 1), "rolling_window"},
		"unknown key":      {minimal + `polling = "1s"`, "polling: unknown key"},
		"data-dir in args": {strings.Replace(minimal, `"--chainid"`, `"--data-dir=/x", "--chainid"`, 1), "--data-dir"},
		"relative node":    {strings.Replace(minimal, `"/srv/gno"`, `"srv"`, 1), "node_dir"},
		"bad duration":     {minimal + `halt_confirm = "soon"`, "halt_confirm"},
		"bool as string":   {minimal + `backup = "yes"`, "backup"},
		"twice":            {minimal + `chain_id = "x"`, "set twice"},
		"no ledger":        {strings.Replace(minimal, `chain_id = "gnoland-1"`, `chain_id = "test-1"`, 1), "ledger_url"},
		"bare value":       {minimal + "rpc = http://x", "line"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.text))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDefaultLedgerIsOnChainMainnetForBothChains(t *testing.T) {
	// Decision D8: chain/mainnet carries the entry from the release on, and
	// holds the onyx folder too (RELEASING.md).
	for chain, dir := range map[string]string{"gnoland-1": "mainnet.gno.land", "onyx-1": "onyx.gno.land"} {
		want := "https://raw.githubusercontent.com/gnolang/gno/refs/heads/chain/mainnet/misc/deployments/" + dir + "/upgrades.json"
		if got := LedgerURLFor(chain); got != want {
			t.Errorf("%s: %s", chain, got)
		}
	}
	if LedgerURLFor("test-1") != "" {
		t.Error("no default for an unknown chain")
	}
}

func TestStringsKeepHashAndEscapes(t *testing.T) {
	c, err := Parse([]byte(minimal + `ledger_url = "/tmp/a#b\"c.json" # comment`))
	if err != nil {
		t.Fatal(err)
	}
	if c.LedgerURL != `/tmp/a#b"c.json` {
		t.Fatalf("ledger_url = %q", c.LedgerURL)
	}
}

func TestWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }
	w, err := ParseWindow("02:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Contains(at(2, 0)) || !w.Contains(at(3, 59)) || w.Contains(at(4, 0)) || w.Contains(at(1, 59)) {
		t.Fatal("plain window")
	}
	w, err = ParseWindow("23:00-01:00")
	if err != nil {
		t.Fatal(err)
	}
	if !w.Contains(at(23, 30)) || !w.Contains(at(0, 30)) || w.Contains(at(1, 0)) || w.Contains(at(22, 59)) {
		t.Fatal("window across midnight")
	}
	// A time in another zone is read in UTC.
	paris := time.FixedZone("CEST", 2*3600)
	if !w.Contains(time.Date(2026, 10, 6, 1, 30, 0, 0, paris)) {
		t.Fatal("01:30 CEST is 23:30 UTC")
	}
	if w.String() != "23:00-01:00 UTC" {
		t.Fatal(w.String())
	}
	if _, err := ParseWindow("04:00-04:00"); err == nil {
		t.Fatal("an empty window must be refused")
	}
}

func TestLoadChecksHomeAndTemplateParses(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "gnovisor"), 0o755); err != nil {
		t.Fatal(err)
	}
	tpl := strings.Replace(Template(home, "gnoland-1"), `rolling_window = ""`, `rolling_window = "05:00-06:00"`, 1)
	if err := os.WriteFile(filepath.Join(home, "gnovisor", FileName), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupDir != filepath.Join(home, "gnovisor", "backups") {
		t.Fatalf("backup_dir = %s", c.BackupDir)
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("a missing file must fail")
	}
	// The template as written must fail until rolling_window is filled.
	if _, err := Parse([]byte(Template(home, "gnoland-1"))); err == nil || !strings.Contains(err.Error(), "rolling_window") {
		t.Fatalf("template without a window: %v", err)
	}
}
