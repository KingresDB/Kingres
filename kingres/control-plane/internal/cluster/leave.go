// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import "github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"

// Clustered reports whether this server is a member of a cluster.
func (s *Service) Clustered() bool { return s.setup.State() == setup.Clustered }
