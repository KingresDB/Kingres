// SPDX-License-Identifier: AGPL-3.0-only

// Command gen writes internal/contract/operations_gen.go from the OpenAPI
// contract. Run it with "go generate ./internal/contract".
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/spec"
)

func main() {
	specPath := flag.String("spec", "", "path of openapi.yaml")
	out := flag.String("out", "operations_gen.go", "output file")
	flag.Parse()
	if err := run(*specPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(specPath, out string) error {
	if specPath == "" {
		return fmt.Errorf("-spec is required")
	}
	doc, err := spec.Load(specPath)
	if err != nil {
		return err
	}
	src, err := doc.OperationsGo()
	if err != nil {
		return err
	}
	// Generated source code is meant to be world-readable.
	if err := os.WriteFile(out, src, 0o644); err != nil { //nolint:gosec
		return fmt.Errorf("writing %s: %w", out, err)
	}
	return nil
}
