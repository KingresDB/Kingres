// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"os"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	pgtest.StopFixture()
	os.Exit(code)
}
