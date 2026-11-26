// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// contractFileBytes returns kingres/api/openapi.yaml: from KINGRES_CONTRACT (where the WSL runner stages a copy for a test binary that runs
// outside the source tree), beside the test file as the compiler recorded it, then relative to the working directory. It reports false only
// when none of them exists, which is the case of a staged binary without the copy.
func contractFileBytes() ([]byte, bool) {
	var paths []string
	if p := os.Getenv("KINGRES_CONTRACT"); p != "" {
		paths = append(paths, p)
	}
	if _, self, _, ok := runtime.Caller(0); ok {
		paths = append(paths, filepath.Join(filepath.Dir(self), "..", "..", "..", "api", "openapi.yaml"))
	}
	paths = append(paths, filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			return b, true
		}
	}
	return nil, false
}

// TestContractRevisionIsTheVersionOfTheContract fails when the revision that kingresd prints and that the database archives name differs from
// info.version of the contract, so the constant cannot go stale when the contract is bumped.
func TestContractRevisionIsTheVersionOfTheContract(t *testing.T) {
	b, ok := contractFileBytes()
	if !ok {
		t.Skip("openapi.yaml is not beside the test file, in the working directory or staged in KINGRES_CONTRACT")
	}
	m := regexp.MustCompile(`(?m)^info:\s*\n(?:[ \t]+.*\n|\s*\n)*?[ \t]+version:[ \t]*"?([0-9]+\.[0-9]+\.[0-9]+)"?[ \t]*\r?$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("info.version not found in openapi.yaml")
	}
	if got := string(m[1]); contractRevision != got {
		t.Errorf("contractRevision is %q but the contract is %q: set ops.ContractRevision to the new version", contractRevision, got)
	}
	if ops.ContractRevision != contractRevision {
		t.Errorf("the archives name contract %q, kingresd prints %q", ops.ContractRevision, contractRevision)
	}
}
