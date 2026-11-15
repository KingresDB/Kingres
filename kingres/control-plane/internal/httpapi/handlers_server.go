// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"path/filepath"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

func (s *Server) serverHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"getThisServer":          s.getThisServer,
		"getThisServerResources": s.getThisServerResources,
		"listThisServerLogs":     s.listThisServerLogs,
		"listLogs":               s.listLogs,
	}
}

func (s *Server) getThisServer(_ *call, w http.ResponseWriter, r *http.Request) error {
	snap := s.deps.Setup.Snapshot()
	started := s.deps.Started
	if started.IsZero() {
		started = s.now()
	}
	out := api.LocalServerInfo{
		Address: s.advertisedAddress(), Hostname: s.deps.Hostname, Version: s.deps.Version,
		NodeState: string(snap.State), UptimeSeconds: int64(s.now().Sub(started).Seconds()),
		IsManagerLeader: false, // set below from the catalog
		Services:        []api.ServiceStatus{},
	}
	if v, err := s.deps.Store.PostgresVersion(r.Context()); err == nil {
		out.PostgresVersion = &v
	}
	if snap.State != setup.Uninitialized {
		out.ServerID = s.localServerID(r.Context())
	}
	out.ClusterID = optStr(snap.ClusterID)
	if out.ServerID != nil {
		if l := s.managerLeader(r.Context()); l != nil && l.ServerID == *out.ServerID {
			out.IsManagerLeader = true
		}
	}
	since := started.UTC()
	out.Services = append(out.Services, api.ServiceStatus{Name: "api", State: "running", Since: &since})
	pg := "stopped"
	if out.PostgresVersion != nil {
		pg = "running"
	}
	out.Services = append(out.Services, api.ServiceStatus{Name: "agent", State: pg})
	mgr := "disabled"
	if snap.State == setup.Clustered {
		mgr = "running"
	}
	out.Services = append(out.Services, api.ServiceStatus{Name: "manager", State: mgr})
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getThisServerResources(_ *call, w http.ResponseWriter, r *http.Request) error {
	disks := map[string]string{"other": s.deps.DataDir}
	if abs, err := filepath.Abs(s.deps.DataDir); err == nil {
		disks["data"] = filepath.Join(abs, "instances")
		delete(disks, "other")
	}
	s.writeJSON(w, r, http.StatusOK, serverinfo.Resources(disks, nil))
	return nil
}
