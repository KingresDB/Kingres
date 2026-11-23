// SPDX-License-Identifier: AGPL-3.0-only

package clustertest

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DownReport says what is known about an instance that does not answer: the markers of the lease directory (a fence, a wait, a
// hand-over), the log of kingresd without its request lines and the logs of the instances. A test that finds the instance of a
// node down reports it with this, so that one finding carries what it takes to judge it (the run directory is removed at the end).
func (n *Node) DownReport() string {
	files, _ := filepath.Glob(filepath.Join(n.DataDir, "lease", "*"))
	for i, f := range files {
		files[i] = filepath.Base(f)
	}
	inst := n.InstanceLogs()
	if len(inst) > 6000 {
		inst = inst[len(inst)-6000:]
	}
	return fmt.Sprintf("node %d (port %d), kingresd running: %v\nlease directory: %s\nkingresd log:\n%s\ninstance logs:\n%s",
		n.Index, n.PGPort, n.Running(), strings.Join(files, " "), n.LogTail(6000), inst)
}

// DoThroughRestart is Do that repeats the answer 503 service_unavailable for up to two minutes. A server answers it while its own
// instance is down for a moment: fenced because a lease round was late (a loaded machine) and started again by the lease manager,
// which a client is meant to wait for (Retry-After). Any other answer is returned at once, and the last 503 after the wait.
func (n *Node) DoThroughRestart(method, path string, body any, headers map[string]string) (int, map[string]any) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st, out := n.Do(method, path, body, headers)
		if st != 503 || out["code"] != "service_unavailable" || time.Now().After(deadline) {
			return st, out
		}
		time.Sleep(2 * time.Second)
	}
}

// DumpGoroutines makes the process of the node print its goroutines into its log and end, so that a failure shows where it was stuck. The node is
// not usable afterwards.
func (n *Node) DumpGoroutines() {
	n.mu.Lock()
	cmd := n.cmd
	n.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	quit(cmd)
	time.Sleep(3 * time.Second)
}
