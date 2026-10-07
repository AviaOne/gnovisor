// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package image extracts one file from a container image in a registry,
// over HTTPS, without Docker.
//
// Why: the gnoland binaries attached to the gno.land releases are built with
// CGO and need glibc 2.38 (measured 2026-10-07 on v1.2.0 to v1.5.0), so they
// do not start on an older system such as Ubuntu 22.04. The gnoland image the
// ledger names for every version (field "image", ref and digest) is built by
// gno.land's Dockerfile with CGO_ENABLED=0: its /usr/bin/gnoland is statically
// linked and starts on any Linux (measured on the same four versions).
//
// Every byte read is checked: the image index against the digest the ledger
// gives (when it gives one), each manifest and each layer against the digest
// that names it (OCI distribution specification, content addressing).
package image

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultRegistry is where the gno.land images live.
const DefaultRegistry = "https://ghcr.io"

const (
	mtOCIIndex      = "application/vnd.oci.image.index.v1+json"
	mtDockerList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtOCIManifest   = "application/vnd.oci.image.manifest.v1+json"
	mtDockerV2      = "application/vnd.docker.distribution.manifest.v2+json"
	mtOCILayerGzip  = "application/vnd.oci.image.layer.v1.tar+gzip"
	mtDockerLayer   = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	mtOCILayerPlain = "application/vnd.oci.image.layer.v1.tar"

	maxManifest = 4 << 20
	maxBlob     = 2 << 30
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Layers    []descriptor `json:"layers"`
}

// Options of a fetch.
type Options struct {
	Registry string       // base URL, DefaultRegistry when empty; https only
	Client   *http.Client // nil: http.DefaultClient
}

type fetcher struct {
	base  string
	repo  string
	c     *http.Client
	token string
}

// Repo returns the repository of a ref such as
// "ghcr.io/gnolang/gno/gnoland:v1.5.0": "gnolang/gno/gnoland".
func Repo(ref string) (string, error) {
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || !strings.Contains(host, ".") {
		return "", fmt.Errorf("image ref %q has no registry host", ref)
	}
	repo, _, _ := strings.Cut(rest, ":")
	repo, _, _ = strings.Cut(repo, "@")
	if repo == "" {
		return "", fmt.Errorf("image ref %q has no repository", ref)
	}
	return repo, nil
}

// File writes the file at name (for example "usr/bin/gnoland") of the image
// ref@digest for plat ("linux/amd64") to dst, mode 0755, and returns its
// sha256 and the digest of the image read. dst must not exist.
//
// With an empty digest the image is read by the tag of ref: gno.land never
// re-pushes a version tag (RELEASING.md, "Container images"), and fills the
// ledger's digest only once its CI has built the image, which can come after
// the halt. The index is then not checked against the ledger, its manifests
// and layers still are against their own digests, and the digest read is
// returned so that the caller can compare it with the ledger later.
func File(ctx context.Context, ref, digest, plat, name, dst string, o Options) (string, string, error) {
	base := o.Registry
	if base == "" {
		base = DefaultRegistry
	}
	if !strings.HasPrefix(base, "https://") {
		return "", "", fmt.Errorf("registry %s: only https is accepted", base)
	}
	repo, err := Repo(ref)
	if err != nil {
		return "", "", err
	}
	c := o.Client
	if c == nil {
		c = http.DefaultClient
	}
	f := &fetcher{base: strings.TrimSuffix(base, "/"), repo: repo, c: c}

	reference := digest
	if reference == "" {
		_, tag, ok := strings.Cut(strings.TrimPrefix(ref, strings.SplitN(ref, "/", 2)[0]+"/"), ":")
		if !ok || tag == "" {
			return "", "", fmt.Errorf("image ref %q has no tag and no digest is given", ref)
		}
		reference = tag
	}
	m, read, err := f.manifest(ctx, reference)
	if err != nil {
		return "", "", err
	}
	if len(m.Manifests) > 0 {
		d, err := pick(m.Manifests, plat)
		if err != nil {
			return "", "", fmt.Errorf("%s@%s: %w", ref, read, err)
		}
		if m, _, err = f.manifest(ctx, d); err != nil {
			return "", "", err
		}
	}
	if len(m.Layers) == 0 {
		return "", "", fmt.Errorf("%s@%s: no layer for %s", ref, read, plat)
	}
	want := strings.TrimPrefix(path.Clean("/"+name), "/")
	// The last layer that names the file holds the version in the image:
	// layers are read from the top down, so the binary, copied after the
	// base system, is usually found in the first few.
	for i := len(m.Layers) - 1; i >= 0; i-- {
		found, gone, err := f.fromLayer(ctx, m.Layers[i], want, dst)
		if err != nil {
			return "", "", err
		}
		if gone {
			break
		}
		if found {
			sum, err := sha256File(dst)
			return sum, read, err
		}
	}
	return "", "", fmt.Errorf("%s@%s: %s not found in the image", ref, read, name)
}

// pick returns the manifest of a platform in an image index. Attestation
// manifests carry the platform unknown/unknown and are never picked.
func pick(list []descriptor, plat string) (string, error) {
	goos, arch, _ := strings.Cut(plat, "/")
	for _, d := range list {
		if d.Platform != nil && d.Platform.OS == goos && d.Platform.Architecture == arch {
			return d.Digest, nil
		}
	}
	return "", fmt.Errorf("no image for %s", plat)
}

func (f *fetcher) get(ctx context.Context, u string, accept []string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if len(accept) > 0 {
			req.Header.Set("Accept", strings.Join(accept, ", "))
		}
		if f.token != "" {
			req.Header.Set("Authorization", "Bearer "+f.token)
		}
		resp, err := f.c.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			if err := f.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
		}
		return resp, nil
	}
}

// authenticate takes an anonymous pull token, as the registry's challenge
// describes it: Bearer realm="...",service="...",scope="...".
func (f *fetcher) authenticate(ctx context.Context, challenge string) error {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("registry asks for an authentication GnoVisor does not handle: %q", challenge)
	}
	p := map[string]string{}
	for _, kv := range splitParams(params) {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	realm := p["realm"]
	if !strings.HasPrefix(realm, "https://") {
		return fmt.Errorf("registry token realm %q is not https", realm)
	}
	q := url.Values{}
	if s := p["service"]; s != "" {
		q.Set("service", s)
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + f.repo + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := f.c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: HTTP %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	f.token = t.Token
	if f.token == "" {
		f.token = t.AccessToken
	}
	if f.token == "" {
		return errors.New("registry token: empty")
	}
	return nil
}

// splitParams splits on commas outside quotes.
func splitParams(s string) []string {
	var out []string
	in, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			in = !in
		case ',':
			if !in {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// manifest reads a manifest by digest, checked against it, or by tag, and
// returns it with the digest of its content.
func (f *fetcher) manifest(ctx context.Context, reference string) (*manifest, string, error) {
	byDigest := strings.HasPrefix(reference, "sha256:")
	if !byDigest && strings.Contains(reference, ":") {
		return nil, "", fmt.Errorf("digest %q is not sha256", reference)
	}
	resp, err := f.get(ctx, f.base+"/v2/"+f.repo+"/manifests/"+reference,
		[]string{mtOCIIndex, mtDockerList, mtOCIManifest, mtDockerV2})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxManifest {
		return nil, "", fmt.Errorf("manifest %s is larger than %d bytes", reference, maxManifest)
	}
	sum := sha256.Sum256(body)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if byDigest && got != reference {
		return nil, "", fmt.Errorf("manifest %s: content has digest %s", reference, got)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", reference, err)
	}
	return &m, got, nil
}

// fromLayer downloads a layer next to dst, checks its digest, and extracts
// want from it. gone is true when the layer deletes want (a whiteout).
func (f *fetcher) fromLayer(ctx context.Context, d descriptor, want, dst string) (found, gone bool, err error) {
	switch d.MediaType {
	case mtOCILayerGzip, mtDockerLayer, mtOCILayerPlain:
	default:
		return false, false, fmt.Errorf("layer %s: media type %q is not handled", d.Digest, d.MediaType)
	}
	if !strings.HasPrefix(d.Digest, "sha256:") {
		return false, false, fmt.Errorf("layer digest %q is not sha256", d.Digest)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".layer-*")
	if err != nil {
		return false, false, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	resp, err := f.get(ctx, f.base+"/v2/"+f.repo+"/blobs/"+d.Digest, nil)
	if err != nil {
		return false, false, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxBlob+1))
	_ = resp.Body.Close()
	if err != nil {
		return false, false, err
	}
	if n > maxBlob {
		return false, false, fmt.Errorf("layer %s is larger than %d bytes", d.Digest, int64(maxBlob))
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d.Digest {
		return false, false, fmt.Errorf("layer %s: content has digest %s", d.Digest, got)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return false, false, err
	}
	var r io.Reader = tmp
	if d.MediaType != mtOCILayerPlain {
		gz, err := gzip.NewReader(tmp)
		if err != nil {
			return false, false, fmt.Errorf("layer %s: %w", d.Digest, err)
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	dir, base := path.Split(want)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, false, nil
		}
		if err != nil {
			return false, false, fmt.Errorf("layer %s: %w", d.Digest, err)
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if name == dir+".wh."+base || name == dir+".wh..wh..opq" {
			return false, true, nil
		}
		if name != want {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return false, false, fmt.Errorf("%s in layer %s is not a regular file", want, d.Digest)
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return false, false, err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxBlob)); err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			return false, false, err
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return false, false, err
		}
		return true, false, out.Close()
	}
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
