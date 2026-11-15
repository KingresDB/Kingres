// SPDX-License-Identifier: PostgreSQL

//go:build linux

package serverinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func cpuTimes() (busy, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	var vals []uint64
	for _, x := range f[1:] {
		v, err := strconv.ParseUint(x, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		vals = append(vals, v)
	}
	for _, v := range vals {
		total += v
	}
	idle := vals[3]
	if len(vals) > 4 {
		idle += vals[4] // iowait
	}
	return total - idle, total, true
}

// Resources samples CPU (over 200 ms), memory and the file system of each
// given directory (purpose -> path).
func Resources(disks map[string]string, quotaBytes *int64) api.ServerResources {
	r := api.ServerResources{SampledAt: time.Now().UTC(), Disks: []api.DiskUsage{}}
	cores := runtime.NumCPU()
	r.CPU.Cores = &cores
	if b1, t1, ok := cpuTimes(); ok {
		time.Sleep(200 * time.Millisecond)
		if b2, t2, ok := cpuTimes(); ok && t2 > t1 {
			p := float64(b2-b1) * 100 / float64(t2-t1)
			r.CPU.UsagePercent = &p
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				r.CPU.LoadAverage1m = &v
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var total, avail int64 = -1, -1
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				continue
			}
			switch f[0] {
			case "MemTotal:":
				total = kb * 1024
			case "MemAvailable:":
				avail = kb * 1024
			}
		}
		if total >= 0 {
			r.Memory.TotalBytes = &total
			if avail >= 0 {
				used := total - avail
				r.Memory.UsedBytes = &used
			}
		}
	}
	r.Memory.QuotaBytes = quotaBytes
	for purpose, path := range disks {
		d := api.DiskUsage{Path: path, Purpose: purpose}
		var st syscall.Statfs_t
		if err := syscall.Statfs(path, &st); err == nil {
			total := int64(st.Blocks) * int64(st.Bsize)      //nolint:gosec // file system sizes fit
			used := total - int64(st.Bavail)*int64(st.Bsize) //nolint:gosec // file system sizes fit
			d.TotalBytes, d.UsedBytes = &total, &used
		}
		r.Disks = append(r.Disks, d)
	}
	return r
}
