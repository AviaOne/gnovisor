// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package testutil builds the fixtures GnoVisor's tests share: a git
// repository standing in for gnolang/gno, an HTTPS server standing in for
// GitHub releases, and a ledger pointing at both.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Release is one version of the fake chain.
type Release struct {
	Kind       string // genesis, upgrade, rolling
	Version    string
	HaltHeight int64 // upgrade only
	Binary     []byte
	Commit     string // filled by Fixture
	// ImageDigest is the digest of the image index, filled by Fixture.
	// NoImageDigest leaves it null in the ledger, as before CI built it.
	ImageDigest   string
	NoImageDigest bool
}

// ImageRepo is the repository of the gnoland image in the fake registry.
const ImageRepo = "gnolang/gno/gnoland"

// Fixture is a fake gnolang/gno: tags in a git repository, binaries behind
// HTTPS.
type Fixture struct {
	Repo     string
	Server   *httptest.Server
	ChainID  string
	Releases []*Release

	mu       sync.Mutex
	files    map[string][]byte
	Hits     map[string]int
	binLayer map[string]string // version -> digest of the layer holding gnoland
}

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// NewFixture creates the repository (one commit and one annotated tag per
// release, each with a gnovm/stdlibs file naming it) and the HTTPS server.
func NewFixture(t testing.TB, chainID string, releases ...*Release) *Fixture {
	t.Helper()
	f := &Fixture{Repo: t.TempDir(), ChainID: chainID, Releases: releases, files: map[string][]byte{}, Hits: map[string]int{}, binLayer: map[string]string{}}
	git(t, f.Repo, "init", "--quiet", "-b", "master")
	for _, r := range releases {
		std := filepath.Join(f.Repo, "gnovm", "stdlibs")
		if err := os.MkdirAll(std, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(std, "VERSION"), []byte(r.Version+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, f.Repo, "add", "-A")
		git(t, f.Repo, "commit", "--quiet", "-m", r.Version)
		git(t, f.Repo, "tag", "-a", r.Version, "-m", r.Version)
		r.Commit = git(t, f.Repo, "rev-parse", "HEAD")
		f.files["/releases/download/"+r.Version+"/gnoland_linux_amd64"] = r.Binary
		f.files["/releases/download/"+r.Version+"/gnoland_linux_arm64"] = r.Binary
		r.ImageDigest = f.addImage(t, r)
	}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		f.Hits[req.URL.Path]++
		b, ok := f.files[req.URL.Path]
		f.mu.Unlock()
		switch {
		case req.URL.Path == "/token":
			// Anonymous pull token, as ghcr.io hands it out.
			if req.URL.Query().Get("scope") != "repository:"+ImageRepo+":pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"pull-token"}`))
			return
		case strings.HasPrefix(req.URL.Path, "/v2/"):
			if req.Header.Get("Authorization") != "Bearer pull-token" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+f.Server.URL+`/token",service="ghcr.io",scope="repository:`+ImageRepo+`:pull"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if strings.Contains(req.URL.Path, "/blobs/") && ok {
				// Blobs are served from another place, as ghcr.io redirects
				// to its storage.
				http.Redirect(w, req, "/storage"+req.URL.Path, http.StatusTemporaryRedirect)
				return
			}
		case strings.HasPrefix(req.URL.Path, "/storage/"):
			f.mu.Lock()
			b, ok = f.files[strings.TrimPrefix(req.URL.Path, "/storage")]
			f.mu.Unlock()
		}
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// Serve replaces what the server returns for a release's binary, in the
// release files and in the image layer, which keeps its old digest: a
// tampered download.
func (f *Fixture) Serve(version string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files["/releases/download/"+version+"/gnoland_linux_amd64"] = b
	f.files["/releases/download/"+version+"/gnoland_linux_arm64"] = b
	f.files[blobPath(f.binLayer[version])] = layerTarGz(map[string][]byte{"usr/bin/gnoland": b})
}

// ImageHits counts the requests made to the fake registry.
func (f *Fixture) ImageHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for p, c := range f.Hits {
		if strings.HasPrefix(p, "/v2/") {
			n += c
		}
	}
	return n
}

func blobPath(digest string) string { return "/v2/" + ImageRepo + "/blobs/" + digest }

func digestOf(b []byte) string { return "sha256:" + SHA256(b) }

// layerTarGz builds a gzip tar layer holding files, in sorted order.
func layerTarGz(files map[string][]byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o755, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		_, _ = tw.Write(files[n])
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// addImage stores a multi-platform gnoland image for r, shaped as the
// gno.land images are: an index with one manifest per platform plus an
// attestation manifest (platform unknown/unknown), a base layer, then the
// layer holding usr/bin/gnoland, then the standard libraries.
func (f *Fixture) addImage(t testing.TB, r *Release) string {
	t.Helper()
	put := func(b []byte) string {
		d := digestOf(b)
		f.files[blobPath(d)] = b
		return d
	}
	base := layerTarGz(map[string][]byte{"etc/alpine-release": []byte("3\n")})
	bin := layerTarGz(map[string][]byte{"usr/bin/gnoland": r.Binary})
	std := layerTarGz(map[string][]byte{"gnoroot/gnovm/stdlibs/VERSION": []byte(r.Version + "\n")})
	cfg := []byte(`{"architecture":"amd64","os":"linux"}`)
	type desc struct {
		MediaType string            `json:"mediaType"`
		Digest    string            `json:"digest"`
		Size      int               `json:"size"`
		Platform  map[string]string `json:"platform,omitempty"`
	}
	layer := func(b []byte) desc {
		return desc{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: put(b), Size: len(b)}
	}
	man := func(layers ...desc) (string, int) {
		b, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config":        desc{MediaType: "application/vnd.oci.image.config.v1+json", Digest: put(cfg), Size: len(cfg)},
			"layers":        layers,
		})
		if err != nil {
			t.Fatal(err)
		}
		d := digestOf(b)
		f.files["/v2/"+ImageRepo+"/manifests/"+d] = b
		return d, len(b)
	}
	binDesc := layer(bin)
	f.binLayer[r.Version] = binDesc.Digest
	img, imgSize := man(layer(base), binDesc, layer(std))
	att, attSize := man(layer(layerTarGz(map[string][]byte{"provenance.json": []byte("{}")})))
	idx, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []desc{
			{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: img, Size: imgSize, Platform: map[string]string{"os": "linux", "architecture": "amd64"}},
			{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: img, Size: imgSize, Platform: map[string]string{"os": "linux", "architecture": "arm64"}},
			{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: att, Size: attSize, Platform: map[string]string{"os": "unknown", "architecture": "unknown"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := digestOf(idx)
	f.files["/v2/"+ImageRepo+"/manifests/"+d] = idx
	f.files["/v2/"+ImageRepo+"/manifests/"+r.Version] = idx // by tag
	return d
}

// SHA256 of b.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Ledger renders upgrades.json for the first n releases (all if n < 0).
func (f *Fixture) Ledger(n int) []byte {
	if n < 0 || n > len(f.Releases) {
		n = len(f.Releases)
	}
	type entry struct {
		Kind           string            `json:"kind"`
		Version        string            `json:"version"`
		Commit         string            `json:"commit"`
		HaltHeight     *int64            `json:"halt_height"`
		HaltTime       *string           `json:"halt_time"`
		HaltMinVersion *string           `json:"halt_min_version"`
		Proposal       *int64            `json:"proposal"`
		Image          map[string]any    `json:"image"`
		Binaries       map[string]string `json:"binaries"`
		RanAs          *string           `json:"ran_as"`
		Release        string            `json:"release"`
	}
	var entries []entry
	for i, r := range f.Releases[:n] {
		var imageDigest any
		if !r.NoImageDigest {
			imageDigest = r.ImageDigest
		}
		e := entry{
			Kind: r.Kind, Version: r.Version, Commit: r.Commit,
			Image:    map[string]any{"ref": "ghcr.io/gnolang/gno/gnoland:" + r.Version, "digest": imageDigest},
			Binaries: map[string]string{},
			Release:  "https://github.com/gnolang/gno/releases/tag/" + r.Version,
		}
		for _, arch := range []string{"amd64", "arm64"} {
			e.Binaries["linux/"+arch] = fmt.Sprintf("%s/releases/download/%s/gnoland_linux_%s?checksum=sha256:%s",
				f.Server.URL, r.Version, arch, SHA256(r.Binary))
		}
		if r.Kind == "upgrade" {
			h := r.HaltHeight
			p := int64(i)
			e.HaltHeight, e.Proposal = &h, &p
		}
		entries = append(entries, e)
	}
	b, err := json.MarshalIndent(map[string]any{
		"schema_version": 1,
		"chain_id":       f.ChainID,
		"genesis_sha256": strings.Repeat("ab", 32),
		"genesis_time":   time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC),
		"upgrades":       entries,
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	return b
}

// Client trusts the fixture's HTTPS server.
func (f *Fixture) Client() *http.Client { return f.Server.Client() }
