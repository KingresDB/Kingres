// SPDX-License-Identifier: AGPL-3.0-only

// Command kingres-watchdog stops a PostgreSQL primary whose lease has run out
// while kingresd, which would have stopped it, is dead or hung (design
// ha-phase5.md, "Fencing by lease"). It is deliberately tiny: standard library
// only, no configuration beyond its flags, no network, no SQL.
//
// kingresd rewrites the lease file after every successful renewal:
//
//	{"seq": 7, "validMs": 4200}
//
// seq changes with every write and validMs says how long the lease still lets
// the primary run. The file carries no time of day. The watchdog notes, with
// its own monotonic clock, when it last saw seq change; once validMs has passed
// since then it writes the fence marker (so that nothing starts the instance as
// a primary without a lease) and stops the instance with an immediate shutdown.
// It exits when the instance is no longer running or after the fence.
//
// A file with "off" says that the instance is not a primary now: it is never fenced on it, however old it is, until a
// file without "off" arrives. A group that merely has no standby is not off: kingresd writes its file again at every
// pass, so a dead or hung kingresd leaves a file that runs out and the instance is fenced (FO-01).
//
// The one exception is an orderly stop: kingresd -stop-instances asks the instance
// for a fast shutdown and then writes the stand-down file, which names the
// postmaster it asked (process id and start time, the first and third line of
// postmaster.pid). From that moment PostgreSQL admits no new session and ends the
// existing ones, so no write can be acknowledged after the lease ends, and the
// watchdog exits instead of turning a slow clean shutdown into a crash. The file
// names one postmaster, so a leftover of an earlier run does not apply to the next.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	leaseFile := flag.String("lease-file", "", "lease file written by kingresd")
	data := flag.String("data", "", "PostgreSQL data directory")
	pgctl := flag.String("pg-ctl", "", "path of pg_ctl")
	marker := flag.String("marker", "", "fence marker file to create before stopping")
	lock := flag.String("lock", "", "lock file: only one watchdog per instance runs")
	standdown := flag.String("standdown", "", "file written by an orderly stop: the instance is shutting down on purpose")
	poll := flag.Duration("poll", 250*time.Millisecond, "how often the lease file is read")
	flag.Parse()
	if *leaseFile == "" || *data == "" || *pgctl == "" || *marker == "" || *lock == "" {
		fmt.Fprintln(os.Stderr, "kingres-watchdog: -lease-file, -data, -pg-ctl, -marker and -lock are required")
		os.Exit(2)
	}
	if !takeLock(*lock) {
		return // another watchdog already watches this instance
	}
	var seq uint64
	var off bool               // kingresd wrote "off": this instance is not a primary now (a demotion), nothing to fence on; a group without a standby is not "off", its file is refreshed
	start := time.Now()        // carries the monotonic reading
	var deadline time.Duration // since start; 0: no lease seen yet, fence at once
	for {
		if !running(*data) {
			return // the instance is stopped: nothing left to watch
		}
		if stoodDown(*standdown, *data) {
			return // an orderly fast shutdown is under way
		}
		if b, err := os.ReadFile(*leaseFile); err == nil {
			var f struct {
				Seq     uint64 `json:"seq"`
				ValidMs int64  `json:"validMs"`
				Off     bool   `json:"off"`
			}
			if json.Unmarshal(b, &f) == nil && f.Seq != seq {
				seq = f.Seq
				off = f.Off
				deadline = time.Since(start) + time.Duration(f.ValidMs)*time.Millisecond
			}
		}
		if !off && time.Since(start) >= deadline {
			fence(*marker, *pgctl, *data)
		}
		time.Sleep(*poll)
	}
}

// fence records that the instance may not start without a lease, then stops it
// immediately. A failure is retried by the next iteration of the loop.
func fence(marker, pgctl, data string) {
	if f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.WriteString("fenced\n")
		_ = f.Sync()
		_ = f.Close()
	}
	fmt.Fprintln(os.Stderr, "kingres-watchdog: the lease has run out, stopping the instance")
	_ = exec.Command(pgctl, "stop", "-D", data, "-m", "immediate", "-w", "-t", "30").Run() //nolint:gosec // fixed program from a flag of kingresd
}

// stoodDown reports whether the stand-down file names the postmaster that runs now.
func stoodDown(file, data string) bool {
	if file == "" {
		return false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	id := identity(data)
	return id != "" && strings.TrimSpace(string(b)) == id
}

// identity is the process id and the start time of the postmaster: the first and
// third line of postmaster.pid.
func identity(data string) string {
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		return ""
	}
	return strings.TrimSpace(lines[0]) + " " + strings.TrimSpace(lines[2])
}

// running reports whether the postmaster named in the data directory's pid file
// is alive (the first line of postmaster.pid is its process id; a stale file of
// a crashed postmaster does not count).
func running(data string) bool {
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]))
	return err == nil && alive(pid)
}
