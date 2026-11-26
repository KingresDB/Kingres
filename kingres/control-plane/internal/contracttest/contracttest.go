// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package contracttest validates real responses against the schemas of the
// REST contract (kingres/api/openapi.yaml) with a JSON Schema 2020-12
// validator. It is imported by tests only. The contract file is found through
// the environment variable KINGRES_CONTRACT (used by the WSL runner) or by
// walking up from the working directory.
package contracttest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spec"
)

// Path returns the location of openapi.yaml.
func Path(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("KINGRES_CONTRACT"); p != "" {
		return p
	}
	dir, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		for _, rel := range []string{"api/openapi.yaml", "kingres/api/openapi.yaml"} {
			p := filepath.Join(dir, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("openapi.yaml not found; set KINGRES_CONTRACT")
	return ""
}

// Load parses the contract.
func Load(t testing.TB) *spec.Document {
	t.Helper()
	doc, err := spec.Load(Path(t))
	if err != nil {
		t.Fatalf("loading the contract: %v", err)
	}
	return doc
}

// Checker validates documents against the contract's schemas.
type Checker struct {
	t    testing.TB
	doc  *spec.Document
	comp *jsonschema.Compiler
}

// NewChecker compiles the contract.
func NewChecker(t testing.TB) *Checker {
	t.Helper()
	doc := Load(t)
	raw, err := doc.JSON()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource("http://contract.test/openapi.json", inst); err != nil {
		t.Fatal(err)
	}
	return &Checker{t: t, doc: doc, comp: c}
}

// Doc returns the parsed contract.
func (c *Checker) Doc() *spec.Document { return c.doc }

// Schema validates body against the component schema name.
func (c *Checker) Schema(name string, body []byte) error {
	c.t.Helper()
	sch, err := c.comp.Compile("http://contract.test/openapi.json#/components/schemas/" + name)
	if err != nil {
		c.t.Fatalf("compiling schema %s: %v", name, err)
	}
	return validate(sch, body)
}

// Response validates body against the application/json response schema the
// contract declares for status of operation opID.
func (c *Checker) Response(opID, status string, body []byte) error {
	c.t.Helper()
	ptr, ok := c.doc.ResponseSchemaPointer(opID, status)
	if !ok {
		c.t.Fatalf("contract has no inline JSON response %s for %s", status, opID)
	}
	sch, err := c.comp.Compile("http://contract.test/openapi.json" + ptr)
	if err != nil {
		c.t.Fatalf("compiling response schema of %s: %v", opID, err)
	}
	return validate(sch, body)
}

func validate(sch *jsonschema.Schema, body []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("response is not JSON: %w", err)
	}
	return sch.Validate(inst)
}
