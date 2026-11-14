// SPDX-License-Identifier: PostgreSQL

//go:build !linux

package serverinfo

import (
	"runtime"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// Resources returns the contract's "not available" for everything except the
// number of CPUs: only Linux is measured.
func Resources(disks map[string]string, quotaBytes *int64) api.ServerResources {
	cores := runtime.NumCPU()
	r := api.ServerResources{SampledAt: time.Now().UTC(), Disks: []api.DiskUsage{}}
	r.CPU.Cores = &cores
	r.Memory.QuotaBytes = quotaBytes
	for purpose, path := range disks {
		r.Disks = append(r.Disks, api.DiskUsage{Path: path, Purpose: purpose})
	}
	return r
}
