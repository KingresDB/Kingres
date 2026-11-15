// SPDX-License-Identifier: PostgreSQL

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
	start := time.Now()        // carries the monotonic reading
	var deadline time.Duration // since start; 0: no lease seen yet, fence at once
	for {
		if !running(*data) {
			return // the instance is stopped: nothing left to watch
		}
		if b, err := os.ReadFile(*leaseFile); err == nil {
			var f struct {
				Seq     uint64 `json:"seq"`
				ValidMs int64  `json:"validMs"`
			}
			if json.Unmarshal(b, &f) == nil && f.Seq != seq {
				seq = f.Seq
				deadline = time.Since(start) + time.Duration(f.ValidMs)*time.Millisecond
			}
		}
		if time.Since(start) >= deadline {
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
