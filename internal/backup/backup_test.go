// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupLeavesOutSecretsAndGnoVisor(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "config", "config.toml"), "cfg")
	write(t, filepath.Join(home, "db", "gnolang.db", "000001.log"), "db")
	write(t, filepath.Join(home, "secrets", "priv_validator_key.json"), "KEY")
	write(t, filepath.Join(home, "gnovisor", "genesis", "bin", "gnoland"), "bin")
	if err := os.Symlink("config/config.toml", filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(home, "gnovisor", "backups")

	dst, err := Make(home, backups, Name(100, "v1.6.0"))
	if err != nil {
		t.Fatal(err)
	}
	if dst != filepath.Join(backups, "100-v1.6.0") {
		t.Fatal(dst)
	}
	b, err := os.ReadFile(filepath.Join(dst, "db", "gnolang.db", "000001.log"))
	if err != nil || string(b) != "db" {
		t.Fatal("db not copied")
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "config/config.toml" {
		t.Fatal("symlink not copied as a link")
	}
	for _, p := range []string{"secrets", "gnovisor"} {
		if _, err := os.Stat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Fatalf("%s must never be in a backup", p)
		}
	}
	st, _ := os.Stat(filepath.Join(dst, "config", "config.toml"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v not kept", st.Mode())
	}

	if _, err := Make(home, backups, Name(100, "v1.6.0")); err == nil {
		t.Fatal("an existing backup must never be overwritten")
	}
	left, _ := filepath.Glob(filepath.Join(backups, ".*"))
	if len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

func TestBackupDirInsideHomeIsSkipped(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "db", "x"), "db")
	backups := filepath.Join(home, "mybackups")
	if _, err := Make(home, backups, "1-v1"); err != nil {
		t.Fatal(err)
	}
	dst, err := Make(home, backups, "2-v2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "mybackups")); !os.IsNotExist(err) {
		t.Fatal("a backup must not contain earlier backups")
	}
}
