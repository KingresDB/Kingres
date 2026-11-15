// SPDX-License-Identifier: AGPL-3.0-only

package cluster

// LogDir is the directory of the log of this server's instance as the instance
// is configured now (an administrator can move it with updateThisServerSettings),
// or "" when the service has no instance manager.
func (s *Service) LogDir() string {
	if s.mgr == nil {
		return ""
	}
	dir, err := s.mgr.LogDir(NodeID)
	if err != nil {
		return ""
	}
	return dir
}
