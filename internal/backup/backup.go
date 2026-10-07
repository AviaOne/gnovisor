// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package backup copies a stopped node directory before a switch.
package backup

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Make copies home into backupDir/<name>, leaving out:
//   - secrets/: a copy of the validator key restored elsewhere risks a
//     double sign (decision D2);
//   - gnovisor/: GnoVisor's own files, binaries and earlier backups;
//   - backupDir itself, when it sits inside home.
//
// The copy is written to a temporary directory and renamed at the end. An
// existing backup of the same name is never overwritten, and GnoVisor never
// deletes a backup.
func Make(home, backupDir, name string) (string, error) {
	home = filepath.Clean(home)
	backupDir = filepath.Clean(backupDir)
	dst := filepath.Join(backupDir, name)
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("backup %s already exists", dst)
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(backupDir, "."+name+".tmp-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()

	skip := map[string]bool{
		filepath.Join(home, "secrets"):  true,
		filepath.Join(home, "gnovisor"): true,
		backupDir:                       true,
	}
	err = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip[path] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		target := filepath.Join(tmp, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel == "." {
				return os.Chmod(tmp, info.Mode().Perm())
			}
			return os.Mkdir(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			// Sockets, pipes and devices have no place in a node directory
			// and cannot be copied meaningfully.
			return fmt.Errorf("%s: unsupported file type %s", path, info.Mode().Type())
		}
	})
	if err != nil {
		return "", fmt.Errorf("backup of %s: %w", home, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	ok = true
	return dst, nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// Name is the backup directory name for a switch: <height>-<version>.
func Name(height int64, version string) string {
	return fmt.Sprintf("%d-%s", height, strings.ReplaceAll(version, "/", "_"))
}
