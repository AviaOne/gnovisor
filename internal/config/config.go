// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package config reads gnovisor.toml.
//
// GnoVisor imports nothing outside the Go standard library, so the file is
// read by a small parser for the subset of TOML it needs: comments, and
// top-level `key = value` lines where value is a basic string, a boolean, or
// an array of basic strings. Anything else is refused with its line number
// rather than guessed.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FileName is the configuration file, inside <node dir>/gnovisor/.
const FileName = "gnovisor.toml"

// DefaultGnorootRepo is where a version's GNOROOT is cloned from (D1).
const DefaultGnorootRepo = "https://github.com/gnolang/gno.git"

// Config is the content of gnovisor.toml.
type Config struct {
	NodeDir        string
	NodeArgs       []string
	ChainID        string
	RPC            string
	LedgerURL      string
	GnorootRepo    string
	PollInterval   time.Duration
	AutoDownload   bool
	Backup         bool
	BackupDir      string
	ShutdownGrace  time.Duration
	HaltConfirm    time.Duration
	RestartTimeout time.Duration
	RollingWindow  Window
}

// LedgerURLFor is the raw ledger of a chain on the chain/mainnet branch of
// gnolang/gno. A release commit carries its ledger entry and may differ from
// master under misc/deployments; the entry is ported to master afterwards
// (RELEASING.md, "Cutting a release" and "Release pages and the upgrade
// ledger"). chain/mainnet also holds the folder of onyx, a testnet that runs
// mainnet's binaries. The operator can point ledger_url elsewhere.
func LedgerURLFor(chainID string) string {
	dir := map[string]string{
		"gnoland-1": "mainnet.gno.land",
		"onyx-1":    "onyx.gno.land",
	}[chainID]
	if dir == "" {
		return ""
	}
	return "https://raw.githubusercontent.com/gnolang/gno/refs/heads/chain/mainnet/misc/deployments/" + dir + "/upgrades.json"
}

// Load reads <home>/gnovisor/gnovisor.toml and checks it.
func Load(home string) (*Config, error) {
	path := filepath.Join(home, "gnovisor", FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if filepath.Clean(c.NodeDir) != filepath.Clean(home) {
		return nil, fmt.Errorf("%s: node_dir %q is not the -home directory %q", path, c.NodeDir, home)
	}
	if !filepath.IsAbs(c.BackupDir) {
		c.BackupDir = filepath.Join(home, "gnovisor", c.BackupDir)
	}
	return c, nil
}

// Parse decodes and checks a configuration.
func Parse(data []byte) (*Config, error) {
	values, err := parseTOML(data)
	if err != nil {
		return nil, err
	}
	c := &Config{
		RPC:            "http://127.0.0.1:26657",
		GnorootRepo:    DefaultGnorootRepo,
		PollInterval:   5 * time.Second,
		AutoDownload:   true,
		Backup:         true,
		BackupDir:      "backups",
		ShutdownGrace:  2 * time.Minute,
		HaltConfirm:    30 * time.Second,
		RestartTimeout: 10 * time.Minute,
	}
	var errs []string
	seen := map[string]bool{}
	str := func(k string, dst *string) {
		v, ok := values[k]
		if !ok {
			return
		}
		seen[k] = true
		s, ok := v.(string)
		if !ok {
			errs = append(errs, k+": expected a string")
			return
		}
		*dst = s
	}
	boolean := func(k string, dst *bool) {
		v, ok := values[k]
		if !ok {
			return
		}
		seen[k] = true
		b, ok := v.(bool)
		if !ok {
			errs = append(errs, k+": expected true or false")
			return
		}
		*dst = b
	}
	duration := func(k string, dst *time.Duration) {
		var s string
		str(k, &s)
		if s == "" {
			return
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			errs = append(errs, k+": expected a positive duration such as \"30s\"")
			return
		}
		*dst = d
	}

	str("node_dir", &c.NodeDir)
	if v, ok := values["node_args"]; ok {
		seen["node_args"] = true
		a, ok := v.([]string)
		if !ok {
			errs = append(errs, "node_args: expected an array of strings")
		}
		c.NodeArgs = a
	}
	str("chain_id", &c.ChainID)
	str("rpc", &c.RPC)
	str("ledger_url", &c.LedgerURL)
	str("gnoroot_repo", &c.GnorootRepo)
	duration("poll_interval", &c.PollInterval)
	boolean("auto_download", &c.AutoDownload)
	boolean("backup", &c.Backup)
	str("backup_dir", &c.BackupDir)
	duration("shutdown_grace", &c.ShutdownGrace)
	duration("halt_confirm", &c.HaltConfirm)
	duration("restart_timeout", &c.RestartTimeout)
	var window string
	str("rolling_window", &window)

	for k := range values {
		if !seen[k] {
			errs = append(errs, k+": unknown key")
		}
	}
	if c.NodeDir == "" || !filepath.IsAbs(c.NodeDir) {
		errs = append(errs, "node_dir: required, an absolute path")
	}
	if len(c.NodeArgs) == 0 {
		errs = append(errs, "node_args: required")
	}
	for _, a := range c.NodeArgs {
		// GnoVisor adds --data-dir itself (specification, section 5).
		if a == "--data-dir" || a == "-data-dir" || strings.HasPrefix(a, "--data-dir=") || strings.HasPrefix(a, "-data-dir=") {
			errs = append(errs, "node_args: must not set --data-dir, gnovisor adds it from node_dir")
		}
	}
	if c.ChainID == "" {
		errs = append(errs, "chain_id: required")
	}
	if c.LedgerURL == "" {
		c.LedgerURL = LedgerURLFor(c.ChainID)
		if c.LedgerURL == "" && c.ChainID != "" {
			errs = append(errs, "ledger_url: required, no default for chain "+c.ChainID)
		}
	}
	if window == "" {
		// No default: one value shared by every operator would restart their
		// nodes together (decision D6).
		errs = append(errs, "rolling_window: required, a UTC range such as \"02:00-04:00\"")
	} else if w, err := ParseWindow(window); err != nil {
		errs = append(errs, "rolling_window: "+err.Error())
	} else {
		c.RollingWindow = w
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return c, nil
}

// Window is a daily UTC time range, start included, end excluded. It may
// wrap past midnight ("23:00-01:00").
type Window struct {
	Start, End time.Duration // since 00:00 UTC
}

// ParseWindow reads "HH:MM-HH:MM".
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, errors.New(`expected "HH:MM-HH:MM"`)
	}
	start, err1 := parseClock(a)
	end, err2 := parseClock(b)
	if err1 != nil || err2 != nil {
		return Window{}, errors.New(`expected "HH:MM-HH:MM"`)
	}
	if start == end {
		return Window{}, errors.New("start and end are equal")
	}
	return Window{Start: start, End: end}, nil
}

func parseClock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	t = t.UTC()
	d := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
	if w.Start < w.End {
		return d >= w.Start && d < w.End
	}
	return d >= w.Start || d < w.End
}

// String gives the window back as "HH:MM-HH:MM UTC".
func (w Window) String() string {
	f := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return f(w.Start) + "-" + f(w.End) + " UTC"
}

// parseTOML reads the subset described in the package comment.
func parseTOML(data []byte) (map[string]any, error) {
	out := map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n)
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if key == "" || strings.ContainsAny(key, " \t\"'[].") {
			return nil, fmt.Errorf("line %d: unsupported key %q", n, key)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("line %d: %s is set twice", n, key)
		}
		v, err := parseValue(raw)
		if err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", n, key, err)
		}
		out[key] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// stripComment removes a # comment that is not inside a string.
func stripComment(s string) string {
	in := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if in {
				i++
			}
		case '"':
			in = !in
		case '#':
			if !in {
				return s[:i]
			}
		}
	}
	return s
}

func parseValue(raw string) (any, error) {
	switch {
	case raw == "true":
		return true, nil
	case raw == "false":
		return false, nil
	case strings.HasPrefix(raw, `"`):
		s, rest, err := parseString(raw)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(rest) != "" {
			return nil, errors.New("unexpected text after the string")
		}
		return s, nil
	case strings.HasPrefix(raw, "["):
		rest := strings.TrimSpace(raw[1:])
		list := []string{}
		for {
			if strings.HasPrefix(rest, "]") {
				if strings.TrimSpace(rest[1:]) != "" {
					return nil, errors.New("unexpected text after the array")
				}
				return list, nil
			}
			s, r, err := parseString(rest)
			if err != nil {
				return nil, errors.New("arrays hold strings only, on one line")
			}
			list = append(list, s)
			rest = strings.TrimSpace(r)
			if strings.HasPrefix(rest, ",") {
				rest = strings.TrimSpace(rest[1:])
			} else if !strings.HasPrefix(rest, "]") {
				return nil, errors.New("expected , or ] in the array")
			}
		}
	}
	return nil, errors.New("expected a \"string\", true, false or an array of strings")
}

// parseString reads one TOML basic string at the start of s.
func parseString(s string) (string, string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", errors.New("expected a string")
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			v, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", fmt.Errorf("bad string %s", s[:i+1])
			}
			return v, s[i+1:], nil
		}
	}
	return "", "", errors.New("unterminated string")
}

// Template is the gnovisor.toml written by `gnovisor init`, to complete.
func Template(nodeDir, chainID string) string {
	return fmt.Sprintf(`# GnoVisor configuration. Every key is described in the README.

node_dir = %q

# Arguments passed to "gnoland start". GnoVisor adds --data-dir itself.
node_args = ["--chainid", %q, "--genesis", "/path/to/genesis.json", "--skip-genesis-sig-verification"]

chain_id = %q

# RPC of the supervised node.
rpc = "http://127.0.0.1:26657"

# Daily UTC range in which a rolling release may be installed. Required, no
# default: pick your own so that validators do not all restart together.
rolling_window = ""

# ledger_url      = "%s"
# gnoroot_repo    = %q
# poll_interval   = "5s"
# auto_download   = true
# backup          = true
# backup_dir      = "backups"
# shutdown_grace  = "2m"
# halt_confirm    = "30s"
# restart_timeout = "10m"
`, nodeDir, chainID, chainID, LedgerURLFor(chainID), DefaultGnorootRepo)
}
