// GnoVisor, by AviaOne.com. Copyright (C) 2026 AviaOne.com.
// SPDX-License-Identifier: AGPL-3.0-only
// Additional terms under section 7 of the AGPL-3.0: see NOTICE.md.

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
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
	// watch reads what the node writes on stderr, passed on unchanged.
	watch *exitWatch
}

// What gnoland writes on stderr just before it exits at a halt, or when it
// refuses to start, the same text from v1.2.0 to v1.8.0 (gnolang/gno):
//   - in block sync, the block after a halt panics in BeginBlock
//     (tm2/pkg/sdk/baseapp.go), on a goroutine with no recover
//     (tm2/pkg/bft/blockchain/reactor.go, poolRoutine): the Go runtime prints
//     the panic and exits with status 2. A node in consensus recovers it and
//     stays up (tm2/pkg/bft/consensus/state.go);
//   - at start, checkNodeStartupParams (gno.land/pkg/gnoland/node_params.go)
//     refuses a binary below the floor of a halt the chain has reached, or
//     one that already meets the floor of a halt still ahead; the error is
//     printed on stderr and the exit status is 1 (tm2/pkg/commands/command.go,
//     Execute).
var (
	haltPanicRE = regexp.MustCompile(`^panic: halt height (\d+) reached, node shutting down`)
	floorRE     = regexp.MustCompile(`does not meet the minimum version "([^"]+)"`)
	earlyRE     = regexp.MustCompile(`is an upgrade intended for halt height (\d+), but the chain is at height (\d+)`)
)

// exitFacts is what the node said about why it exits.
type exitFacts struct {
	// HaltPanic is the halt height of a node that exited at a halt.
	HaltPanic int64
	// Floor is the version a vote requires, for a binary refused below it.
	Floor string
	// EarlyHalt and EarlyHeight: a binary refused because it is meant for
	// the halt at EarlyHalt, while the node is at EarlyHeight.
	EarlyHalt, EarlyHeight int64
}

func (f exitFacts) String() string {
	return fmt.Sprintf("%d/%s/%d/%d", f.HaltPanic, f.Floor, f.EarlyHalt, f.EarlyHeight)
}

// exitWatch scans stderr line by line for exitFacts.
type exitWatch struct {
	mu    sync.Mutex
	line  []byte
	facts exitFacts
}

// maxLine bounds the memory a line without a newline can take; the lines
// looked for are short and start a line.
const maxLine = 64 << 10

func (w *exitWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		w.scan(w.line[:i])
		w.line = w.line[i+1:]
	}
	if len(w.line) > maxLine {
		w.line = w.line[len(w.line)-maxLine:]
	}
	return len(p), nil
}

// flush scans a last line written without a newline.
func (w *exitWatch) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.line) > 0 {
		w.scan(w.line)
		w.line = nil
	}
}

func (w *exitWatch) scan(l []byte) {
	if m := haltPanicRE.FindSubmatch(l); m != nil {
		w.facts.HaltPanic, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	if m := floorRE.FindSubmatch(l); m != nil {
		w.facts.Floor = string(m[1])
	}
	if m := earlyRE.FindSubmatch(l); m != nil {
		w.facts.EarlyHalt, _ = strconv.ParseInt(string(m[1]), 10, 64)
		w.facts.EarlyHeight, _ = strconv.ParseInt(string(m[2]), 10, 64)
	}
}

func (w *exitWatch) get() exitFacts {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.facts
}

// watchStderr passes the node's stderr on to GnoVisor's own and reads it.
// The node then writes into a pipe, not into GnoVisor's stderr itself: if
// GnoVisor dies, the node's next write ends it too. Under systemd nothing
// changes: with the default KillMode, the whole service stops with GnoVisor.
func watchStderr(cmd *exec.Cmd) *exitWatch {
	w := &exitWatch{}
	cmd.Stderr = io.MultiWriter(os.Stderr, w)
	return w
}

// wait waits for the node, then reads its last line.
func (n *node) wait() {
	n.err = n.cmd.Wait() // returns once stderr is fully copied
	n.watch.flush()
	close(n.done)
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
	w := watchStderr(cmd)
	// Own process group: a Ctrl-C in a terminal reaches GnoVisor only, which
	// then stops the node itself, in order.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	n := &node{cmd: cmd, done: make(chan struct{}), watch: w}
	go n.wait()
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
	w := watchStderr(cmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	n := &node{cmd: cmd, done: make(chan struct{}), container: name, dock: d, watch: w}
	go n.wait()
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
