// SPDX-License-Identifier: AGPL-3.0-only

// Package serverinfo reports what the operating system and the instance's log
// files know about this server: resources (Linux /proc and statfs; every
// value is null where it cannot be measured) and the PostgreSQL log.
package serverinfo

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// MaxLogBytes is how much of each log file is read (its end).
const MaxLogBytes = 1 << 20

// MaxLogFiles bounds the number of log files read.
const MaxLogFiles = 8

var lineRE = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)?)(?: [A-Z]{3,5}| [+-]\d{2,4})? \[(\d+)\] ([A-Z0-9]+):\s+(.*)$`)

// ReadLogs reads the log files of the instance in dir (the directory is
// chosen by the caller from its own configuration, never from a request) and
// returns the entries newest first. Only regular files named postgresql-*.log
// are read, and at most MaxLogBytes of the end of each.
func ReadLogs(dir string, serverID *string, instanceID int32) ([]api.LogEntry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "postgresql-*.log"))
	if err != nil {
		return nil, err
	}
	type fi struct {
		path string
		mod  time.Time
	}
	var list []fi
	for _, f := range files {
		st, err := os.Lstat(f)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		list = append(list, fi{f, st.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].mod.After(list[j].mod) })
	if len(list) > MaxLogFiles {
		list = list[:MaxLogFiles]
	}
	var out []api.LogEntry
	for _, f := range list {
		es, err := readFile(f.path, serverID, instanceID)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

func readFile(path string, serverID *string, instanceID int32) ([]api.LogEntry, error) {
	f, err := os.Open(path) //nolint:gosec // a file found by Glob in the instance's own log directory
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > MaxLogBytes {
		if _, err := f.Seek(-MaxLogBytes, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var out []api.LogEntry
	for sc.Scan() {
		m := lineRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue // continuation lines and a cut first line
		}
		ts, ok := parseTime(m[1])
		if !ok {
			continue
		}
		id := instanceID
		out = append(out, api.LogEntry{Timestamp: ts, Level: level(m[3]), Component: "postgres", ServerID: serverID, InstanceID: &id, Message: strings.TrimSpace(m[4])})
	}
	return out, nil
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func level(s string) string {
	switch s {
	case "WARNING":
		return "warning"
	case "ERROR", "FATAL", "PANIC":
		return "error"
	case "DEBUG1", "DEBUG2", "DEBUG3", "DEBUG4", "DEBUG5":
		return "debug"
	}
	return "info"
}
