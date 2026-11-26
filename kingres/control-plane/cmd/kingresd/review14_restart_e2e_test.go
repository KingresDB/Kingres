// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"testing"
)

// DX14-02: the quota is the sum over the live files, also after kingresd restarted. An upload that holds its room before the restart still holds it after.
func TestStagingQuotaHoldsAfterARestart(t *testing.T) {
	e := startClusterWith(t, true)
	e.expect(t, 200, "PATCH", "/settings/cluster", map[string]any{"dataTransfer": map[string]any{"enabled": true, "retentionHours": 24, "maxFileBytes": 1 << 30, "stagingQuotaBytes": 800 << 20, "maxConcurrent": 4}})
	first := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20})
	e.stopManager(t)
	e.startManager(t)
	e.login(t, adminUser, adminPass)
	st, out, _ := e.call("POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20}, nil)
	if st != 409 || problemCode(out) != "transfer_quota_exceeded" {
		t.Errorf("a second upload beyond the quota after the restart: %d %v", st, out)
	}
	// The first upload is still there and its delete frees the room.
	e.expect(t, 204, "DELETE", "/data-uploads/"+first["uploadId"].(string), nil)
	again := e.expect(t, 201, "POST", "/data-uploads", map[string]any{"purpose": "tableImport", "sizeBytes": 700 << 20})
	e.expect(t, 204, "DELETE", "/data-uploads/"+again["uploadId"].(string), nil)
}
