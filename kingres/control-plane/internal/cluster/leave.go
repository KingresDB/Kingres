// SPDX-License-Identifier: PostgreSQL

package cluster

import "github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"

// Clustered reports whether this server is a member of a cluster.
func (s *Service) Clustered() bool { return s.setup.State() == setup.Clustered }
