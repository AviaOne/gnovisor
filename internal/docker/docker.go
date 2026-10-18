// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

// Package docker runs the gnoland image of a version through the docker
// command line, for a node that runs in a container instead of from a binary
// (decision D17).
//
// GnoVisor imports nothing outside the Go standard library, so it drives the
// docker CLI rather than the Docker API.
//
// The gnoland image is gno.land's own (Dockerfile, target gnoland): its
// entrypoint is /usr/bin/gnoland and it carries its GNOROOT, /gnoroot, at the
// version of its binary, so a version needs nothing else.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client runs the docker command line.
type Client struct {
	Bin string // "docker" when empty
}

func (c Client) bin() string {
	if c.Bin == "" {
		return "docker"
	}
	return c.Bin
}

func (c Client) out(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Pull pulls an image. A reference by digest is checked by docker against
// that digest.
func (c Client) Pull(ctx context.Context, ref string) error {
	_, err := c.out(ctx, "pull", "--quiet", ref)
	return err
}

// Digest returns the digest docker recorded for an image of repo pulled
// locally under ref, "" when the image is not present. For a multi-platform
// image it is the digest of the image index, the one the ledger gives.
func (c Client) Digest(ctx context.Context, ref, repo string) (string, error) {
	s, err := c.out(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", ref)
	if err != nil {
		return "", err
	}
	var digests []string
	if err := json.Unmarshal([]byte(s), &digests); err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w", ref, err)
	}
	for _, d := range digests {
		r, dg, ok := strings.Cut(d, "@")
		if ok && (r == repo || strings.HasSuffix(r, "/"+repo)) {
			return dg, nil
		}
	}
	return "", fmt.Errorf("docker image inspect %s: no digest for %s", ref, repo)
}

// Has reports whether the image is present locally.
func (c Client) Has(ctx context.Context, ref string) bool {
	_, err := c.out(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return err == nil
}

// Run is the command that runs gnoland in the image, attached, so that its
// output reaches GnoVisor's own and its end is the node's end. The node
// directory is mounted at the same path, so the node arguments written for a
// binary (--data-dir, --genesis inside it) stay valid. The host network keeps
// the RPC and p2p addresses of the node unchanged. The container runs as the
// user of GnoVisor, so the files it writes stay the node user's.
func (c Client) Run(name, image string, uid, gid int, home string, extra, gnolandArgs []string) *exec.Cmd {
	args := []string{"run", "--rm", "--name", name, "--network", "host",
		"--user", strconv.Itoa(uid) + ":" + strconv.Itoa(gid),
		"--volume", home + ":" + home}
	args = append(args, extra...)
	args = append(args, image)
	args = append(args, gnolandArgs...)
	return exec.Command(c.bin(), args...)
}

// Stop stops the container, SIGTERM then SIGKILL after grace, as for a
// binary: gnoland stops cleanly on SIGTERM.
func (c Client) Stop(name string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace+time.Minute)
	defer cancel()
	_, err := c.out(ctx, "stop", "--time", strconv.Itoa(int(grace.Seconds())), name)
	return err
}

// Remove removes a container left by a previous run of GnoVisor that ended
// without stopping it; a missing container is not an error.
func (c Client) Remove(ctx context.Context, name string) error {
	_, err := c.out(ctx, "rm", "--force", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// ErrNoDigest is returned for an image reference that is not pinned by
// digest where one is required.
var ErrNoDigest = errors.New("the image must be given by digest: <repository>@sha256:<64 hex>")

// SplitDigest returns the repository and the digest of "repo@sha256:...".
func SplitDigest(ref string) (repo, digest string, err error) {
	repo, digest, ok := strings.Cut(ref, "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", "", ErrNoDigest
	}
	return repo, digest, nil
}
