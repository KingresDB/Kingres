// SPDX-License-Identifier: PostgreSQL

// Command kingres-verify checks the digests and inclusion proofs of the
// KingresDB tamper-evident ledger without trusting the database. See
// kingres/verifier/README.md.
package main

import (
	"os"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr, version))
}
