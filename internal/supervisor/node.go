// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/AviaOne/gnovisor/internal/docker"
	"github.com/AviaOne/gnovisor/internal/layout"
)

// node is one run of gnoland.
type node struct {
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
	stopped bool // GnoVisor asked it to stop
	// Docker mode: the container the docker client runs, stopped through
	// docker so that the container never outlives a stop.
	container string
	dock      *docker.Client
}

// startNode runs <dir>/bin/gnoland start --data-dir <home> <args> with
// GNOROOT=<dir>/gnoroot. dir is the resolved version directory, not the
// current link, so a later switch never changes a running node's GNOROOT.
// GNOROOT is always set: release binaries are built without -trimpath and
// otherwise look for GNOROOT in the build machine's path
// (.github/workflows/release-chain-tag.yml in gnolang/gno).
func startNode(dir, home string, args []string) (*node, error) {
	argv := append([]string{"start", "--data-dir", home}, args...)
	cmd := exec.Command(layout.Binary(dir), argv...)
	cmd.Env = append(withoutGnoroot(os.Environ()), "GNOROOT="+layout.Gnoroot(dir))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group: a Ctrl-C in a terminal reaches GnoVisor only, which
	// then stops the node itself, in order.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	n := &node{cmd: cmd, done: make(chan struct{})}
	go func() {
		n.err = cmd.Wait()
		close(n.done)
	}()
	return n, nil
}

// startContainer runs gnoland start --data-dir <home> <args> in the image of
// a version, in a container named name (decision D17). The image carries its
// own GNOROOT. The docker client stays attached: its output is the node's,
// and its exit is the container's.
func startContainer(d *docker.Client, name, image, home string, extra, args []string) (*node, error) {
	argv := append([]string{"start", "--data-dir", home}, args...)
	cmd := d.Run(name, image, os.Getuid(), os.Getgid(), home, extra, argv)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	n := &node{cmd: cmd, done: make(chan struct{}), container: name, dock: d}
	go func() {
		n.err = cmd.Wait()
		close(n.done)
	}()
	return n, nil
}

func withoutGnoroot(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if len(kv) >= 8 && kv[:8] == "GNOROOT=" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stop sends SIGTERM to the node's own pid, waits grace, then SIGKILL.
// gnoland stops cleanly on SIGTERM (gno.land/cmd/gnoland/root.go and
// start.go lines 316 to 332).
func (n *node) stop(grace time.Duration) error {
	n.stopped = true
	select {
	case <-n.done:
		return nil
	default:
	}
	if n.container != "" {
		// docker stop sends SIGTERM to gnoland, then SIGKILL after grace;
		// the attached client exits with the container.
		err := n.dock.Stop(n.container, grace)
		select {
		case <-n.done:
		case <-time.After(time.Minute):
			_ = n.cmd.Process.Kill()
			<-n.done
		}
		if err != nil {
			return fmt.Errorf("stopping container %s: %w", n.container, err)
		}
		return nil
	}
	if err := n.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-n.done:
		return nil
	case <-time.After(grace):
	}
	if err := n.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	<-n.done
	return fmt.Errorf("node did not stop within %s and was killed", grace)
}

func (n *node) exitDescription() string {
	if n.err == nil {
		return "exit code 0"
	}
	var ee *exec.ExitError
	if errors.As(n.err, &ee) {
		return ee.String()
	}
	return n.err.Error()
}
