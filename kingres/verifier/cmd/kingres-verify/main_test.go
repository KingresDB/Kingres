// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"testing"
)

// asMain is set in the environment of a child that is this test binary used as
// the program: the tests of the standard output start it with descriptors that
// os/exec cannot give (a closed one).
const asMain = "KINGRES_VERIFY_TEST_AS_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(asMain) == "1" {
		os.Exit(realMain())
	}
	os.Exit(m.Run())
}
