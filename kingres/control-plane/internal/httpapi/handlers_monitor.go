// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

func (s *Server) monitorHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listAlerts":       s.listAlerts,
		"acknowledgeAlert": s.acknowledgeAlert,
		"queryMetrics":     s.queryMetrics,
	}
}

var metricUnits = map[string]struct {
	unit string
	sum  bool
}{
	"cpuUsagePercent": {"percent", false}, "memoryUsedBytes": {"bytes", true}, "diskUsedBytes": {"bytes", true},
	"connections": {"count", true}, "replicationLagBytes": {"bytes", false}, "replicationLagSeconds": {"seconds", false},
	"databaseSizeBytes": {"bytes", true}, "transactionsPerSecond": {"countPerSecond", true},
	"queriesPerSecond": {"countPerSecond", true}, "bucketsUnderReplicated": {"count", true},
}

func init() {
	// The counters of the client entry points (contract 0.7.0), per server and summed over servers.
	for _, p := range []string{"client", "clientStandby"} {
		for _, n := range []string{"ConnectionsAccepted", "ConnectionsActive", "ConnectionsRefused", "ConnectFailures", "ConnectionsMoved", "ConnectionsIdleClosed"} {
			metricUnits[p+n] = struct {
				unit string
				sum  bool
			}{"count", true}
		}
		for _, n := range []string{"BytesIn", "BytesOut"} {
			metricUnits[p+n] = struct {
				unit string
				sum  bool
			}{"bytes", true}
		}
	}
}

var errUnavailable = errors.New("backing store unavailable")

// metricsEndpoint serves the Prometheus text format at /metrics (outside the
// REST contract). It requires authentication: a bearer token of a session with
// the kingres_stats role or better, or the scrape token configured with
// metrics-token. Without either the endpoint answers 401.
func (s *Server) metricsEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	unauth := func() {
		w.Header().Set("WWW-Authenticate", `Bearer realm="kingresd-metrics"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		unauth()
		return
	}
	allowed := s.deps.MetricsToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.MetricsToken)) == 1
	if !allowed {
		if sess, err := s.deps.Sessions.Lookup(token); err == nil && sess.Kind == auth.KindToken {
			// The roles are the current ones, not those of the sign-in (CP-02).
			if cur, perr := s.currentRoles(r, sess.Username); perr == nil && auth.Allows(cur, auth.RoleStats) {
				allowed = true
			}
		}
	}
	if !allowed {
		unauth()
		return
	}
	if s.deps.Setup.State() == setup.Uninitialized || s.deps.Metrics == nil {
		http.Error(w, "no metrics", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.deps.Metrics.WritePrometheus(w)
}
