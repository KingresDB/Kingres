// SPDX-License-Identifier: PostgreSQL

package httpapi

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
