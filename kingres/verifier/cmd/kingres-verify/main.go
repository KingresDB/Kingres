// SPDX-License-Identifier: AGPL-3.0-only

// Command kingres-verify checks the digests and inclusion proofs of the
// KingresDB tamper-evident ledger without trusting the database. See
// kingres/verifier/README.md.
package main

import (
	"fmt"
	"os"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/cli"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/stdio"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

func main() {
	os.Exit(realMain())
}

// realMain refuses to run when the standard output is the null device that the
// runtime opens in place of a closed descriptor (L12-04, see internal/stdio):
// a verdict written there is lost, and a run that cannot show its verdict is
// not status 0. It then runs the command.
func realMain() int {
	if stdio.Refused(os.Stdout, os.Getenv) {
		fmt.Fprintln(os.Stderr, "kingres-verify: the standard output is closed (it is the null device opened for reading and writing, which is what the runtime puts in place of a closed descriptor): the verdict could not be shown, so nothing was run. Give a file or a pipe; for no output at all write >/dev/null. A caller that gave the null device on purpose (Python subprocess.DEVNULL, daemon(3), a service manager) sets "+stdio.NoOutputEnv+"=1 and gets the status as the only answer")
		return cli.ExitUsage
	}
	return cli.Run(os.Args[1:], os.Stdout, os.Stderr, version)
}
