// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package docker

import (
	"strings"
	"testing"
)

func TestSplitDigest(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	if r, g, err := SplitDigest("ghcr.io/x/y@" + d); err != nil || r != "ghcr.io/x/y" || g != d {
		t.Fatalf("%s %s %v", r, g, err)
	}
	for _, bad := range []string{"ghcr.io/x/y:v1", "ghcr.io/x/y@sha256:abc", "ghcr.io/x/y@md5:" + strings.Repeat("a", 64)} {
		if _, _, err := SplitDigest(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestRunArguments(t *testing.T) {
	cmd := Client{}.Run("c1", "img@sha256:x", 1000, 1001, "/srv/gno", []string{"--volume", "/g:/g:ro"}, []string{"start", "--data-dir", "/srv/gno"})
	got := strings.Join(cmd.Args, " ")
	want := "docker run --rm --name c1 --network host --user 1000:1001 --volume /srv/gno:/srv/gno --volume /g:/g:ro img@sha256:x start --data-dir /srv/gno"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
