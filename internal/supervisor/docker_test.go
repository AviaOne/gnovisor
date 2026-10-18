// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AviaOne/gnovisor/internal/config"
	"github.com/AviaOne/gnovisor/internal/layout"
	"github.com/AviaOne/gnovisor/internal/local"
	"github.com/AviaOne/gnovisor/internal/testutil"
)

// The fake docker is this test binary started in helper mode. It knows the
// images listed in images.json, records every call, and runs an image as the
// fake node of the image's version, in its own process, which stands for the
// container: the pid is recorded under the container name.

type fakeImage struct {
	Version string `json:"version"`
	Repo    string `json:"repo"`
	Digest  string `json:"digest"`
}

func fakeDocker() int {
	dir := os.Getenv("FAKE_DOCKER_DIR")
	args := os.Args[1:]
	if f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = fmt.Fprintln(f, strings.Join(args, " "))
		_ = f.Close()
	}
	images := map[string]fakeImage{}
	b, _ := os.ReadFile(filepath.Join(dir, "images.json"))
	_ = json.Unmarshal(b, &images)
	pulled := func() map[string]bool {
		m := map[string]bool{}
		b, _ := os.ReadFile(filepath.Join(dir, "pulled"))
		for _, l := range strings.Fields(string(b)) {
			m[l] = true
		}
		return m
	}
	pull := func(img fakeImage) {
		f, _ := os.OpenFile(filepath.Join(dir, "pulled"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		_, _ = fmt.Fprintln(f, img.Digest)
		_ = f.Close()
	}
	last := args[len(args)-1]
	container := func(name string) string { return filepath.Join(dir, "container."+name) }
	switch args[0] {
	case "pull":
		img, ok := images[last]
		if !ok {
			_, _ = fmt.Fprintln(os.Stderr, "manifest unknown")
			return 1
		}
		pull(img)
		return 0
	case "image":
		img, ok := images[last]
		if !ok || !pulled()[img.Digest] {
			_, _ = fmt.Fprintln(os.Stderr, "Error: No such image: "+last)
			return 1
		}
		if strings.Contains(strings.Join(args, " "), "RepoDigests") {
			_, _ = fmt.Fprintf(os.Stdout, "[%q]\n", img.Repo+"@"+img.Digest)
		} else {
			_, _ = fmt.Fprintln(os.Stdout, "sha256:id")
		}
		return 0
	case "run":
		name := ""
		for i, a := range args {
			if a == "--name" {
				name = args[i+1]
			}
			img, ok := images[a]
			if !ok {
				continue
			}
			if !pulled()[img.Digest] {
				pull(img) // docker run pulls a missing image
			}
			if _, err := os.Stat(container(name)); err == nil {
				_, _ = fmt.Fprintln(os.Stderr, "Conflict. The container name is already in use")
				return 125
			}
			_ = os.WriteFile(container(name), []byte(strconv.Itoa(os.Getpid())), 0o644)
			f, _ := os.OpenFile(filepath.Join(dir, "runs"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			_, _ = fmt.Fprintln(f, strings.Join(args, " "))
			_ = f.Close()
			exe, _ := os.Executable()
			env := append(os.Environ(), "GNOVISOR_FAKE_DOCKER=0", "GNOVISOR_FAKE_NODE=1", "FAKE_VERSION="+img.Version, "GNOROOT=/gnoroot")
			err := syscall.Exec(exe, append([]string{exe}, args[i+1:]...), env)
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_, _ = fmt.Fprintln(os.Stderr, "Unable to find image")
		return 125
	case "stop", "rm":
		b, err := os.ReadFile(container(last))
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "Error response from daemon: No such container: "+last)
			return 1
		}
		pid, _ := strconv.Atoi(string(b))
		sig := syscall.SIGTERM
		if args[0] == "rm" {
			sig = syscall.SIGKILL
		}
		_ = syscall.Kill(pid, sig)
		for i := 0; i < 500 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
		_ = os.Remove(container(last))
		return 0
	}
	_, _ = fmt.Fprintln(os.Stderr, "fake docker: unknown command "+args[0])
	return 1
}

// setupDocker turns the harness into a node run in a container.
func (h *harness) setupDocker() {
	h.t.Helper()
	dir := h.t.TempDir()
	h.t.Setenv("FAKE_DOCKER_DIR", dir)
	images := map[string]fakeImage{}
	for _, r := range h.f.Releases {
		img := fakeImage{Version: r.Version, Repo: "ghcr.io/" + testutil.ImageRepo, Digest: r.ImageDigest}
		images["ghcr.io/"+testutil.ImageRepo+":"+r.Version] = img
		images["ghcr.io/"+testutil.ImageRepo+"@"+r.ImageDigest] = img
	}
	h.dockerDir = dir
	h.writeImages(images)
	exe, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(fmt.Sprintf("#!/bin/sh\nexec env GNOVISOR_FAKE_DOCKER=1 %q \"$@\"\n", exe)), 0o755); err != nil {
		h.t.Fatal(err)
	}
	h.cfg.Runtime, h.cfg.DockerBin = config.RuntimeDocker, bin
	h.cfg.Container = config.DefaultContainer("test-1", h.lay.Home)
	h.cfg.DockerArgs = []string{"--volume", "/srv/genesis.json:/srv/genesis.json:ro"}
}

func (h *harness) writeImages(images map[string]fakeImage) {
	b, _ := json.Marshal(images)
	if err := os.WriteFile(filepath.Join(h.dockerDir, "images.json"), b, 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) dockerFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.dockerDir, name))
	return string(b)
}

func (h *harness) stop() {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		h.t.Fatal("gnovisor did not stop")
	}
}

func TestDockerScheduledHaltSwitchesTheImage(t *testing.T) {
	h := newHarnessMode(t, true, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("preparation", func() bool { return h.logged("ready for the halt at height 30, version v1.6.0") })
	h.waitFor("switch", func() bool { return h.chainHeight() >= 35 })

	if v := h.currentVersion(); v != "v1.6.0" {
		t.Fatalf("current = %s", v)
	}
	dir, _ := h.lay.CurrentDir()
	info, err := layout.ReadInfo(dir)
	want := "ghcr.io/" + testutil.ImageRepo + "@" + h.f.Releases[1].ImageDigest
	if err != nil || !info.Docker() || info.ImageRef != want {
		t.Fatalf("version.json = %+v %v", info, err)
	}
	if _, err := os.Stat(layout.Binary(dir)); !os.IsNotExist(err) {
		t.Fatal("a container version holds no binary")
	}
	if starts := h.nodeStarts(); strings.Join(starts, ",") != "v1.5.0,v1.6.0" {
		t.Fatalf("node runs = %v", starts)
	}
	runs := strings.Split(strings.TrimSpace(h.dockerFile("runs")), "\n")
	if len(runs) != 2 {
		t.Fatalf("docker runs = %q", runs)
	}
	user := fmt.Sprintf("--user %d:%d", os.Getuid(), os.Getgid())
	for _, w := range []string{
		"run --rm --name " + h.cfg.Container + " --network host " + user,
		"--volume " + h.lay.Home + ":" + h.lay.Home,
		"--volume /srv/genesis.json:/srv/genesis.json:ro",
		want + " start --data-dir " + h.lay.Home + " --chainid test-1",
	} {
		if !strings.Contains(runs[1], w) {
			t.Fatalf("docker run lacks %q:\n%s", w, runs[1])
		}
	}
	if !strings.Contains(h.dockerFile("calls"), "stop --time 2 "+h.cfg.Container) {
		t.Fatalf("the node must be stopped through docker stop:\n%s", h.dockerFile("calls"))
	}
	backup := filepath.Join(h.cfg.BackupDir, "30-v1.5.0")
	if _, err := os.Stat(filepath.Join(backup, "db", "data")); err != nil {
		t.Fatal("no backup of db")
	}
	if _, err := os.Stat(filepath.Join(backup, "secrets")); !os.IsNotExist(err) {
		t.Fatal("secrets/ must never be backed up")
	}
	h.stop()
	if _, err := os.Stat(filepath.Join(h.dockerDir, "container."+h.cfg.Container)); !os.IsNotExist(err) {
		t.Fatal("a stop of gnovisor must stop the container")
	}
}

func TestDockerHaltWithoutVoteDoesNotStopTheNode(t *testing.T) {
	h := newHarnessMode(t, true, 2, rel("genesis", "v1.5.0", 0), rel("upgrade", "v1.6.0", 30))
	h.setControl(func(c *control) { c.Ranges["v1.5.0"] = [2]int64{1, 30} })
	h.run()
	h.waitFor("height 30", func() bool { return h.chainHeight() == 30 })
	time.Sleep(100 * time.Millisecond)
	h.setControl(func(c *control) { c.Ranges["v1.5.0"] = [2]int64{1, 0} })
	h.waitFor("blocks after 30", func() bool { return h.chainHeight() >= 40 })
	if v := h.currentVersion(); v != "v1.5.0" || len(h.nodeStarts()) != 1 {
		t.Fatalf("current = %s, runs %v", v, h.nodeStarts())
	}
}

func TestDockerContainerLeftBehindIsRemovedFirst(t *testing.T) {
	// GnoVisor killed without stopping its container: the container keeps
	// running and holds the name. The next start removes it.
	h := newHarnessMode(t, true, 1, rel("genesis", "v1.5.0", 0))
	if err := os.WriteFile(filepath.Join(h.dockerDir, "container."+h.cfg.Container), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.waitFor("blocks", func() bool { return h.chainHeight() >= 3 })
	if !strings.Contains(h.dockerFile("calls"), "rm --force "+h.cfg.Container) {
		t.Fatalf("calls:\n%s", h.dockerFile("calls"))
	}
}

func TestDockerVersionAddedByHand(t *testing.T) {
	// The private security fix, received as an image.
	old := PendingLedgerInterval
	PendingLedgerInterval = 200 * time.Millisecond
	t.Cleanup(func() { PendingLedgerInterval = old })

	h := newHarnessMode(t, true, 1, rel("genesis", "v1.5.0", 0))
	h.setControl(func(c *control) {
		c.HaltHeight = 30
		c.Ranges["v1.5.0"] = [2]int64{1, 30}
		c.Ranges["v1.5.9"] = [2]int64{31, 0}
	})
	digest := "sha256:" + strings.Repeat("9", 64)
	ref := "registry.example/fix@" + digest
	images := map[string]fakeImage{}
	_ = json.Unmarshal([]byte(h.dockerFile("images.json")), &images)
	images[ref] = fakeImage{Version: "v1.5.9", Repo: "registry.example/fix", Digest: digest}
	h.writeImages(images)
	dir := h.lay.Upgrade("v1.5.9")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := layout.WriteInfo(dir, layout.Info{Version: "v1.5.9", Image: digest, Runtime: layout.RuntimeDocker, ImageRef: ref}); err != nil {
		t.Fatal(err)
	}
	f, _ := local.Read(h.lay)
	f.Upgrades = append(f.Upgrades, local.Entry{Version: "v1.5.9", HaltHeight: 30, Image: ref, Added: time.Now()})
	if err := local.Write(h.lay, f); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.waitFor("switch to the image added by hand", func() bool { return h.chainHeight() >= 32 })
	if h.currentVersion() != "v1.5.9" || !strings.Contains(h.dockerFile("runs"), ref+" start") {
		t.Fatalf("current = %s\nruns:\n%s", h.currentVersion(), h.dockerFile("runs"))
	}
}

func TestDockerRefusesAVersionPreparedAsABinary(t *testing.T) {
	h := newHarness(t, 1, rel("genesis", "v1.5.0", 0))
	h.cfg.Runtime = config.RuntimeDocker
	h.run()
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "runtime") {
			t.Fatalf("err = %v", err)
		}
		h.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("gnovisor did not refuse")
	}
}

func TestDockerMissingDigestPullsByTag(t *testing.T) {
	// The ledger lists the version before gno.land filled the image digest:
	// the image is pulled by its tag and pinned by the digest docker read.
	up := rel("upgrade", "v1.6.0", 30)
	up.NoImageDigest = true
	h := newHarnessMode(t, true, 2, rel("genesis", "v1.5.0", 0), up)
	h.setControl(func(c *control) { c.HaltHeight = 30 })
	h.run()
	h.waitFor("switch", func() bool { return h.chainHeight() >= 32 })
	if !strings.Contains(h.dockerFile("calls"), "pull --quiet ghcr.io/"+testutil.ImageRepo+":v1.6.0") {
		t.Fatalf("calls:\n%s", h.dockerFile("calls"))
	}
	dir, _ := h.lay.CurrentDir()
	if info, _ := layout.ReadInfo(dir); info.ImageRef != "ghcr.io/"+testutil.ImageRepo+"@"+h.f.Releases[1].ImageDigest {
		t.Fatalf("version.json = %+v", info)
	}
}
