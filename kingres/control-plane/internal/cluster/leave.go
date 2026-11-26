// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import "github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"

// Clustered reports whether this server is a member of a cluster.
func (s *Service) Clustered() bool { return s.setup.State() == setup.Clustered }
