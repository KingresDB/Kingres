// SPDX-License-Identifier: PostgreSQL

package pgstore

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

func setupFixture(t *testing.T) (string, error) { return pgtest.Fixture(t), nil }
