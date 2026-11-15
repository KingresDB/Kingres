// SPDX-License-Identifier: PostgreSQL

package cluster

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scram"
)

// RoleVerifier returns the stored password verifier of a role that can log in,
// read through the service connection of the managed instance (the connection
// that the join and the role copy use for the same table, which works from the
// first moment the instance exists). A role that does not exist, cannot log in
// or has no password yields scram.ErrNotSCRAM: for the proof of a forwarded
// request they all mean "nothing can be proven".
func (s *Service) RoleVerifier(ctx context.Context, role string) (string, error) {
	c, err := s.connect(ctx, "postgres")
	if err != nil {
		return "", fmt.Errorf("connecting to read a verifier: %w", err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var stored *string
	err = c.QueryRow(ctx, `SELECT rolpassword FROM pg_catalog.pg_authid WHERE rolname = $1 AND rolcanlogin`, role).Scan(&stored)
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && stored == nil:
		return "", scram.ErrNotSCRAM
	case err != nil:
		return "", fmt.Errorf("reading a verifier: %w", err)
	}
	return *stored, nil
}
