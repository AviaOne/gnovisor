// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepo(t *testing.T) {
	for ref, want := range map[string]string{
		"ghcr.io/gnolang/gno/gnoland:v1.5.0":         "gnolang/gno/gnoland",
		"ghcr.io/gnolang/gno/gnoland@sha256:" + "ab": "gnolang/gno/gnoland",
	} {
		if got, err := Repo(ref); err != nil || got != want {
			t.Errorf("%s: %q %v", ref, got, err)
		}
	}
	if _, err := Repo("gnoland:v1.5.0"); err == nil {
		t.Error("a ref without registry host must be refused")
	}
}

func TestSplitParams(t *testing.T) {
	got := splitParams(`realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	if len(got) != 3 || !strings.HasSuffix(got[2], `pull,push"`) {
		t.Fatalf("%q", got)
	}
}

func TestPickSkipsAttestations(t *testing.T) {
	type p = struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	list := []descriptor{
		{Digest: "att", Platform: &p{"unknown", "unknown"}},
		{Digest: "amd", Platform: &p{"linux", "amd64"}},
	}
	if d, err := pick(list, "linux/amd64"); err != nil || d != "amd" {
		t.Fatalf("%s %v", d, err)
	}
	if _, err := pick(list, "linux/arm64"); err == nil {
		t.Fatal("missing platform accepted")
	}
}

func tgz(t *testing.T, files ...[2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o755, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func dg(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

// serve stores a single-platform image whose layers are given bottom up.
func serve(t *testing.T, layers ...[]byte) (string, *httptest.Server) {
	t.Helper()
	files := map[string][]byte{}
	var ls []string
	for _, l := range layers {
		files["/v2/r/x/blobs/"+dg(l)] = l
		ls = append(ls, `{"mediaType":"`+mtOCILayerGzip+`","digest":"`+dg(l)+`","size":1}`)
	}
	m := []byte(`{"schemaVersion":2,"mediaType":"` + mtOCIManifest + `","layers":[` + strings.Join(ls, ",") + `]}`)
	files["/v2/r/x/manifests/"+dg(m)] = m
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return dg(m), srv
}

func TestTopLayerWinsAndWhiteoutHides(t *testing.T) {
	d, srv := serve(t,
		tgz(t, [2]string{"usr/bin/gnoland", "old"}),
		tgz(t, [2]string{"./usr/bin/gnoland", "new"}),
	)
	dst := filepath.Join(t.TempDir(), "gnoland")
	o := Options{Registry: srv.URL, Client: srv.Client()}
	if _, read, err := File(context.Background(), "ghcr.io/r/x:v1", d, "linux/amd64", "usr/bin/gnoland", dst, o); err != nil || read != d {
		t.Fatal(read, err)
	}

	d, srv = serve(t,
		tgz(t, [2]string{"usr/bin/gnoland", "old"}),
		tgz(t, [2]string{"usr/bin/.wh.gnoland", ""}),
	)
	o = Options{Registry: srv.URL, Client: srv.Client()}
	_, _, err := File(context.Background(), "ghcr.io/r/x:v1", d, "linux/amd64", "usr/bin/gnoland", filepath.Join(t.TempDir(), "g"), o)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("a deleted file must not be taken from a lower layer: %v", err)
	}
}

func TestWrongManifestDigestIsRefused(t *testing.T) {
	_, srv := serve(t, tgz(t, [2]string{"usr/bin/gnoland", "x"}))
	o := Options{Registry: srv.URL, Client: srv.Client()}
	_, _, err := File(context.Background(), "ghcr.io/r/x:v1", "sha256:"+strings.Repeat("0", 64), "linux/amd64", "usr/bin/gnoland", filepath.Join(t.TempDir(), "g"), o)
	if err == nil {
		t.Fatal("an unknown digest must fail")
	}
}
