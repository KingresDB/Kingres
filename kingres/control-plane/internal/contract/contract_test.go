// SPDX-License-Identifier: PostgreSQL

package contract

import (
	"bytes"
	"os"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spec"
)

const specPath = "../../../api/openapi.yaml"

// TestGeneratedTableIsCurrent fails when operations_gen.go does not match the
// contract: run "go generate ./internal/contract".
func TestGeneratedTableIsCurrent(t *testing.T) {
	doc, err := spec.Load(contractFile())
	if err != nil {
		t.Fatal(err)
	}
	want, err := doc.OperationsGo()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("operations_gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("operations_gen.go is stale; run: go generate ./internal/contract")
	}
}

func TestOperationHelpers(t *testing.T) {
	ops := Operations()
	if len(ops) == 0 {
		t.Fatal("no operations")
	}
	ids := map[string]bool{}
	for _, op := range ops {
		if ids[op.ID] {
			t.Errorf("duplicate operation id %s", op.ID)
		}
		ids[op.ID] = true
		if op.Role == "" || op.Method == "" || op.Path == "" {
			t.Errorf("incomplete operation %+v", op)
		}
	}
	tests := []struct {
		id                         string
		mutating, public, setupTok bool
	}{
		{"getHealth", false, true, false},
		{"login", true, true, false},
		{"listDatabases", false, false, false},
		{"initCluster", true, false, true},
		{"getTask", false, false, false},
		{"checkSetupToken", true, false, true},
		{"createRole", true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			var op Operation
			for _, o := range ops {
				if o.ID == tc.id {
					op = o
				}
			}
			if op.ID == "" {
				t.Fatalf("operation %s not found", tc.id)
			}
			if op.Mutating() != tc.mutating || op.Public() != tc.public || op.Accepts(SchemeSetupToken) != tc.setupTok {
				t.Errorf("%s: mutating=%v public=%v setup=%v", tc.id, op.Mutating(), op.Public(), op.Accepts(SchemeSetupToken))
			}
		})
	}
}

func TestAuditActions(t *testing.T) {
	for _, a := range []string{"login", "loginFailed", "setupTokenRejected", "autoFailover", "createDatabase", "executeQuery"} {
		if !IsAuditAction(a) {
			t.Errorf("%s must be an audit action", a)
		}
	}
	for _, a := range []string{"getHealth", "listDatabases", "checkSetupToken", "createInclusionProof", "previewRebalancePlan", "database.create"} {
		if IsAuditAction(a) {
			t.Errorf("%s must not be an audit action", a)
		}
	}
}

// contractFile is the contract; the WSL runner sets KINGRES_CONTRACT.
func contractFile() string {
	if p := os.Getenv("KINGRES_CONTRACT"); p != "" {
		return p
	}
	return specPath
}
