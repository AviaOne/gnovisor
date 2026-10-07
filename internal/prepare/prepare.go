// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package prepare makes a version ready to run: its gnoland binary and its
// GNOROOT, both checked against the ledger.
//
// A version needs both: the node reads GNOROOT/gnovm/stdlibs
// (gno.land/pkg/gnoland/app.go), and VALIDATOR.md asks for a GNOROOT at the
// same tag as the binary.
//
// The binary is taken from the gnoland image the ledger names for the
// version, checked against the image digest the ledger gives. The binaries
// attached to the releases are linked against glibc 2.38 and do not start
// on an older system; the binary of the image is static and starts on any
// Linux (see package image).
package prepare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/AviaOne/gnovisor/internal/image"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/ledger"
)

// BinaryInImage is where the gnoland image holds the binary (gno.land
// Dockerfile, target gnoland: COPY ... /usr/bin/gnoland).
const BinaryInImage = "usr/bin/gnoland"

// Options of a preparation.
type Options struct {
	Platform     string // "linux/amd64"
	AutoDownload bool
	GnorootRepo  string
	HTTPClient   *http.Client // nil: a client with a 30 minute timeout
	Registry     string       // image registry, image.DefaultRegistry when empty
	Log          func(format string, args ...any)
}

func (o Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Version makes e ready under l and returns its directory. version.json is
// written last: its presence is what makes a directory a ready version, so
// an interrupted preparation never leaves a half-ready one.
func Version(ctx context.Context, l layout.Layout, e *ledger.Entry, o Options) (string, error) {
	_, release, err := e.Binary(o.Platform)
	if err != nil {
		return "", err
	}
	if dir := l.VersionDir(e.Version); dir != "" {
		i, err := layout.Verify(dir)
		if err != nil {
			return "", err
		}
		err = matches(i, e, release)
		if err == nil {
			return dir, nil
		}
		if i.Image == "" || dir != l.Upgrade(e.Version) {
			return "", fmt.Errorf("%s: %w", dir, err)
		}
		// The ledger now names another image for this version than the one
		// prepared ahead of the halt (its digest was filled after the image
		// was read by its tag, or gno.land changed it): the version is
		// prepared again from the image the ledger names. Never the
		// directory in service: a target is always another version.
		o.logf("%s: %v; preparing it again", dir, err)
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
	}

	dir := l.Upgrade(e.Version)
	if err := os.MkdirAll(l.Upgrades(), 0o755); err != nil {
		return "", err
	}
	info := layout.Info{Version: e.Version, Commit: e.Commit}
	deposited := layout.Binary(dir)
	if _, err := os.Stat(deposited); err == nil {
		// A binary the operator put there has priority (decision of
		// 2026-10-05), but only with the ledger's digest.
		got, err := layout.SHA256File(deposited)
		if err != nil {
			return "", err
		}
		if got != release {
			return "", fmt.Errorf("%s has sha256 %s, the ledger says %s for %s", deposited, got, release, e.Version)
		}
		o.logf("using the binary deposited in %s", deposited)
		if err := os.Chmod(deposited, 0o755); err != nil {
			return "", err
		}
		info.SHA256 = got
	} else {
		if !o.AutoDownload {
			return "", fmt.Errorf("%s is not prepared and auto_download is off: put the binary in %s", e.Version, deposited)
		}
		tmp, err := os.MkdirTemp(l.Upgrades(), "."+e.Version+".tmp-")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		if info.SHA256, info.Image, err = fromImage(ctx, e, o, layout.Binary(tmp)); err != nil {
			return "", err
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", err
		}
	}

	if err := gnoroot(ctx, o, e, dir); err != nil {
		return "", err
	}
	if err := layout.WriteInfo(dir, info); err != nil {
		return "", err
	}
	if _, err := layout.Verify(dir); err != nil {
		return "", err
	}
	o.logf("prepared %s in %s", e.Version, dir)
	return dir, nil
}

// Into fills dir, which must not exist, with version e: the binary from its
// image and its GNOROOT. It serves `gnovisor init -version`, for the version
// a node runs when GnoVisor is installed.
func Into(ctx context.Context, dir string, e *ledger.Entry, o Options) (layout.Info, error) {
	info := layout.Info{Version: e.Version, Commit: e.Commit}
	if _, err := os.Lstat(dir); err == nil {
		return info, fmt.Errorf("%s already exists", dir)
	}
	var err error
	if info.SHA256, info.Image, err = fromImage(ctx, e, o, layout.Binary(dir)); err != nil {
		return info, err
	}
	if err := gnoroot(ctx, o, e, dir); err != nil {
		return info, err
	}
	if err := layout.WriteInfo(dir, info); err != nil {
		return info, err
	}
	_, err = layout.Verify(dir)
	return info, err
}

// matches checks a prepared version against its ledger entry.
func matches(i layout.Info, e *ledger.Entry, release string) error {
	if i.Commit != e.Commit {
		return fmt.Errorf("prepared from commit %s, the ledger says %s", i.Commit, e.Commit)
	}
	if i.Image != "" {
		// A digest the ledger does not give yet cannot be compared: the
		// image was read by its tag, which gno.land never re-pushes.
		if e.Image.Digest != nil && *e.Image.Digest != i.Image {
			return fmt.Errorf("binary taken from image %s, the ledger now gives %s", i.Image, *e.Image.Digest)
		}
		return nil
	}
	if i.SHA256 != release {
		return fmt.Errorf("binary sha256 %s, the ledger says %s", i.SHA256, release)
	}
	return nil
}

// fromImage writes the gnoland binary of e's image to dst and returns its
// sha256 and the digest of the image read. While the ledger gives no digest
// (gno.land fills it once its CI has built the image), the image is read by
// its tag rather than leaving a halted node waiting.
func fromImage(ctx context.Context, e *ledger.Entry, o Options, dst string) (string, string, error) {
	digest := ""
	if e.Image.Digest != nil {
		digest = *e.Image.Digest
	}
	c := o.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Minute}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", "", err
	}
	sum, read, err := image.File(ctx, e.Image.Ref, digest, o.Platform, BinaryInImage, dst, image.Options{Registry: o.Registry, Client: c})
	if err != nil {
		return "", "", fmt.Errorf("%s: binary from image %s: %w", e.Version, e.Image.Ref, err)
	}
	if digest == "" {
		o.logf("the ledger gives no image digest for %s yet: image read by its tag, digest %s", e.Version, read)
	}
	o.logf("took the gnoland binary of %s from image %s@%s, sha256 %s", e.Version, e.Image.Ref, read, sum)
	return sum, read, nil
}

// gnoroot puts in dir a git copy of the GNOROOT at the tag, its commit
// compared with the ledger (decision D1: the ledger publishes no digest of a
// source archive).
func gnoroot(ctx context.Context, o Options, e *ledger.Entry, dir string) error {
	if err := os.RemoveAll(layout.Gnoroot(dir)); err != nil {
		return err
	}
	tmpRoot, err := os.MkdirTemp(dir, ".gnoroot.tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()
	if err := cloneAt(ctx, o.GnorootRepo, e.Version, e.Commit, tmpRoot); err != nil {
		return err
	}
	return os.Rename(tmpRoot, layout.Gnoroot(dir))
}

// cloneAt copies repo at tag into dst and checks that the tag points at
// commit.
func cloneAt(ctx context.Context, repo, tag, commit, dst string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git is required to prepare a GNOROOT and was not found")
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, "git", "-c", "advice.detachedHead=false",
		"clone", "--quiet", "--depth", "1", "--single-branch", "--branch", tag, "--", repo, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone %s at %s: %v: %s", repo, tag, err, strings.TrimSpace(string(out)))
	}
	out, err = exec.CommandContext(ctx, "git", "-C", dst, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse in %s: %w", dst, err)
	}
	if got := strings.TrimSpace(string(out)); got != commit {
		return fmt.Errorf("tag %s of %s is commit %s, the ledger says %s", tag, repo, got, commit)
	}
	return nil
}
