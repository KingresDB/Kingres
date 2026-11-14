// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"path/filepath"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
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

// logs reads the instance log; the directory comes from the configuration.
func (s *Server) logs(r *http.Request, serverID *string) (api.LogPage, error) {
	pp := newParams(r)
	level := pp.enum("level", "debug", "info", "warning", "error")
	component := pp.enum("component", "api", "agent", "manager", "router", "etcd", "patroni", "postgres", "metrics")
	from, to := pp.timeQuery("from"), pp.timeQuery("to")
	page := pp.page()
	if p := pp.err(); p != nil {
		return api.LogPage{}, p
	}
	page2 := api.LogPage{Items: []api.LogEntry{}}
	if s.deps.LogDir == "" || (component != "" && component != "postgres") {
		return page2, nil // only the instance log exists in this version
	}
	all, err := serverinfo.ReadLogs(s.deps.LogDir, serverID, 1)
	if err != nil {
		s.log.Error("reading the instance log", "request_id", stateOf(r).id, "error", err)
		return page2, problem.New(problem.InternalError, "The log could not be read.")
	}
	var sel []api.LogEntry
	for _, e := range all {
		if (level != "" && e.Level != level) || (!from.IsZero() && e.Timestamp.Before(from)) || (!to.IsZero() && e.Timestamp.After(to)) {
			continue
		}
		sel = append(sel, e)
	}
	items, next, err := store.Paginate(sel, page)
	if err != nil {
		return page2, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
	}
	if items == nil {
		items = []api.LogEntry{}
	}
	return api.LogPage{Items: items, NextCursor: next}, nil
}

func (s *Server) listThisServerLogs(_ *call, w http.ResponseWriter, r *http.Request) error {
	id := s.localServerID(r.Context())
	out, err := s.logs(r, id)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// listLogs serves the cluster logs; this version has the local server only, so
// a filter on another server or on a task gives an empty page.
func (s *Server) listLogs(_ *call, w http.ResponseWriter, r *http.Request) error {
	id := s.localServerID(r.Context())
	if want := r.URL.Query().Get("serverId"); want != "" && (id == nil || *id != want) || r.URL.Query().Get("taskId") != "" {
		s.writeJSON(w, r, http.StatusOK, api.LogPage{Items: []api.LogEntry{}})
		return nil
	}
	out, err := s.logs(r, id)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
