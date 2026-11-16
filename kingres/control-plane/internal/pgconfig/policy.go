// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"fmt"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// Levels of a value.
const (
	LevelCluster     = "cluster"
	LevelServerGroup = "serverGroup"
	LevelServer      = "server"
	LevelDatabase    = "database"
)

// Levels in the order of precedence of the three that go into the override file, wide to narrow.
var fileLevels = []string{LevelCluster, LevelServerGroup, LevelServer}

// Kinds of a parameter.
const (
	KindSettable = "settable"
	KindManaged  = "managed"
	KindRefused  = "refused"
)

// Class says what may be done with a parameter.
type Class struct {
	Kind string
	// Reason is set for managed and refused parameters.
	Reason instance.Reason
	// Levels are the levels a value may be set at.
	Levels []string
	// ClusterOnly is why the value must be equal on every server ("" when it need not be).
	ClusterOnly string
	// Hidden is true when PostgreSQL hides the value from an ordinary database user.
	Hidden bool
}

// Allows reports whether a value may be set at the level.
func (c Class) Allows(level string) bool {
	for _, l := range c.Levels {
		if l == level {
			return true
		}
	}
	return false
}

// clusterOnly lists the parameters that must be equal on every server (design config-phase13.md,
// "Cluster-only parameters"; the facts are in the README of kingres_shard, "Nodes must agree" and
// "Session settings", and node_conn.c). Keys are lower case.
var clusterOnly = map[string]string{
	"timezone":                              "a function evaluated on another node (now()::timestamp, date_trunc) gives another result when the zone differs; the sharding extension does not force the zone on its remote sessions",
	"timezone_abbreviations":                "it is part of the meaning of a time zone",
	"log_timezone":                          "the logs of the cluster read as one",
	"lc_monetary":                           "formatting on the node that evaluates a function (to_char) depends on it",
	"lc_numeric":                            "formatting on the node that evaluates a function (to_char) depends on it",
	"lc_time":                               "formatting on the node that evaluates a function (to_char) depends on it",
	"default_text_search_config":            "to_tsvector without a configuration depends on it",
	"datestyle":                             "parsing and formatting of dates on the node that evaluates a statement",
	"intervalstyle":                         "formatting of intervals on the node that evaluates a statement",
	"extra_float_digits":                    "formatting of floating point numbers",
	"standard_conforming_strings":           "the meaning of string literals",
	"bytea_output":                          "formatting of binary values",
	"default_transaction_isolation":         "a transaction takes the default of the node it lands on",
	"default_transaction_read_only":         "a transaction takes the default of the node it lands on",
	"default_transaction_deferrable":        "a transaction takes the default of the node it lands on",
	"kingres_shard.default_bucket_count":    "the number of buckets is part of how a table is distributed",
	"kingres_shard.enable_routing":          "a node that does not route returns other rows than one that does",
	"kingres_shard.enable_distributed_scan": "it decides how a statement reads other nodes",
	"kingres_ledger.block_interval":         "blocks closed by different rules make a chain that is hard to verify",
	"kingres_ledger.max_block_transactions": "blocks closed by different rules make a chain that is hard to verify",
	"max_prepared_transactions":             "two-phase commit across nodes needs a usable value on every node",
}

// ClusterOnlyReason returns why the parameter must be equal on every server, "" when it need not be.
func ClusterOnlyReason(name string) string { return clusterOnly[strings.ToLower(name)] }

// ClusterOnlyNames returns the names of the cluster-only table, sorted.
func ClusterOnlyNames() []string {
	out := make([]string, 0, len(clusterOnly))
	for k := range clusterOnly {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// refusedReason gives the reason of a parameter that PostgreSQL hides from an ordinary database user
// (GUC_SUPERUSER_ONLY): it runs a command, loads code, names a file or holds a connection string.
// A name that is not here is refused as superuser-only all the same (fail safe: a parameter that a
// later version or an extension marks so stays refused until someone decides).
var refusedReason = map[string]instance.Reason{
	"archive_command":           {Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."},
	"archive_cleanup_command":   {Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."},
	"restore_command":           {Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."},
	"recovery_end_command":      {Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."},
	"ssl_passphrase_command":    {Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."},
	"archive_library":           {Code: instance.ReasonLoadsCode, Text: "it loads code into the server; set it on the server."},
	"session_preload_libraries": {Code: instance.ReasonLoadsCode, Text: "it loads code into every session; set it on the server."},
	"local_preload_libraries":   {Code: instance.ReasonLoadsCode, Text: "it loads code into every session; set it on the server."},
	"oauth_validator_libraries": {Code: instance.ReasonLoadsCode, Text: "it loads code into the server; set it on the server."},
	"output_plugin_libraries":   {Code: instance.ReasonLoadsCode, Text: "it loads code into the server; set it on the server."},
	"dynamic_library_path":      {Code: instance.ReasonLoadsCode, Text: "it decides where code is loaded from; set it on the server."},
	"extension_control_path":    {Code: instance.ReasonLoadsCode, Text: "it decides where extensions are loaded from; set it on the server."},
	"jit_provider":              {Code: instance.ReasonLoadsCode, Text: "it loads code into the server; set it on the server."},
	"ssl_library":               {Code: instance.ReasonLoadsCode, Text: "it names the TLS library of the server."},
	"krb_server_keyfile":        {Code: instance.ReasonNamesFile, Text: "it names a key file; set it on the server."},
	"ssl_dh_params_file":        {Code: instance.ReasonNamesFile, Text: "it names a file; set it on the server."},
	"hosts_file":                {Code: instance.ReasonNamesFile, Text: "it names the file of host-based TLS settings (PostgreSQL 19); set it on the server."},
	"ssl_sni":                   {Code: instance.ReasonTLS, Text: "it changes how the cluster's own connections negotiate TLS (PostgreSQL 19); set it on the server."},
	"ssl_ciphers":               {Code: instance.ReasonTLS, Text: "it changes how the cluster's own connections negotiate TLS; set it on the server."},
	"ssl_groups":                {Code: instance.ReasonTLS, Text: "it changes how the cluster's own connections negotiate TLS; set it on the server."},
	"ssl_max_protocol_version":  {Code: instance.ReasonTLS, Text: "it changes how the cluster's own connections negotiate TLS; set it on the server."},
	"ssl_tls13_ciphers":         {Code: instance.ReasonTLS, Text: "it changes how the cluster's own connections negotiate TLS; set it on the server."},
}

// refusedByPolicy are parameters refused although PostgreSQL does not hide them.
var refusedByPolicy = map[string]instance.Reason{
	"fsync":                         {Code: instance.ReasonUnsafe, Text: "with it off a crash can corrupt a server that the cluster believes holds acknowledged commits."},
	"full_page_writes":              {Code: instance.ReasonUnsafe, Text: "with it off a crash can corrupt a server that the cluster believes holds acknowledged commits."},
	"kingres_shard.fault_injection": {Code: instance.ReasonDeveloper, Text: "a test hook of the sharding extension."},
	"kingres_shard.forwarded":       {Code: instance.ReasonSessionInternal, Text: "set by the router on forwarded sessions."},
	"kingres_shard.forward_token":   {Code: instance.ReasonSessionInternal, Text: "set by the router on forwarded sessions."},
}

// Classify decides what may be done with a parameter. hidden is true when PostgreSQL hides it from a
// role without pg_read_all_settings (found at run time, see Agent.Hidden).
func Classify(s Setting, hidden bool) Class {
	name := strings.ToLower(s.Name)
	c := Class{Hidden: hidden, ClusterOnly: clusterOnly[name]}
	if r, ok := instance.IsManaged(name); ok {
		c.Kind, c.Reason = KindManaged, r
		return c
	}
	if s.Context == "internal" {
		c.Kind, c.Reason = KindRefused, instance.Reason{Code: instance.ReasonInternal, Text: "PostgreSQL does not let anyone change it."}
		return c
	}
	if r, ok := refusedByPolicy[name]; ok {
		c.Kind, c.Reason = KindRefused, r
		return c
	}
	if r, ok := refusedReason[name]; ok {
		c.Kind, c.Reason, c.Hidden = KindRefused, r, true
		return c
	}
	if r, ok := codeNameReason(name); ok {
		c.Kind, c.Reason, c.Hidden = KindRefused, r, true
		return c
	}
	if hidden {
		r, ok := refusedReason[name]
		if !ok {
			r = instance.Reason{Code: instance.ReasonSuperuserOnly, Text: "PostgreSQL hides it from ordinary database users (it can run commands, load code or name files); set it on the server."}
		}
		c.Kind, c.Reason = KindRefused, r
		return c
	}
	if strings.HasPrefix(strings.ToLower(s.Category), "developer options") {
		c.Kind, c.Reason = KindRefused, instance.Reason{Code: instance.ReasonDeveloper, Text: "a developer option; it exists to break safety checks."}
		return c
	}
	c.Kind = KindSettable
	c.Levels = []string{LevelCluster}
	if c.ClusterOnly == "" {
		c.Levels = append(c.Levels, LevelServerGroup, LevelServer)
	}
	// PostgreSQL allows ALTER DATABASE ... SET for these contexts only (checked: log_timezone and
	// shared_buffers are refused with "cannot be changed now / without restarting").
	if s.Context == "user" || s.Context == "superuser" {
		c.Levels = append(c.Levels, LevelDatabase)
	}
	return c
}

// CheckLevel returns nil when a value may be set at the level, else the problem code and a sentence.
func CheckLevel(s Setting, c Class, level string) (code, msg string) {
	switch c.Kind {
	case KindManaged:
		return "config_parameter_managed", fmt.Sprintf("%s is managed by kingresd: %s", s.Name, c.Reason.Text)
	case KindRefused:
		return "config_parameter_refused", fmt.Sprintf("%s cannot be set from the console: %s", s.Name, c.Reason.Text)
	}
	if !c.Allows(level) {
		if c.ClusterOnly != "" && (level == LevelServerGroup || level == LevelServer) {
			return "config_level_not_allowed", fmt.Sprintf("%s must be equal on every server (%s), so it is set for the cluster or for a database.", s.Name, c.ClusterOnly)
		}
		return "config_level_not_allowed", fmt.Sprintf("%s cannot be set at the %s level (its context is %s).", s.Name, level, s.Context)
	}
	return "", ""
}

// Floor is the smallest value kingresd's own needs allow for a parameter, in the base unit of the
// parameter (a count). replicas is the cluster's replica count.
func Floor(name string, replicas int) (min int64, reason string, ok bool) {
	switch strings.ToLower(name) {
	case "max_prepared_transactions":
		return 1, "the sharding extension needs two-phase commit (at least 1)", true
	case "max_wal_senders":
		return int64(4 + replicas), fmt.Sprintf("every standby holds a sender on its primary and building one takes more (4 + the replica count, %d)", 4+replicas), true
	case "max_replication_slots":
		return int64(2 + replicas), fmt.Sprintf("every standby holds a slot on its primary (2 + the replica count, %d)", 2+replicas), true
	}
	return 0, "", false
}

// Override is a value stored for a level.
type Override struct {
	Level string
	Scope string
	Value string
}

// Pick returns the override that decides the value on a server: the server's, else its server
// group's, else the cluster's (a narrower level overrides a wider one). ok is false when none
// applies. group is "" for a server that is in no group.
func Pick(overrides []Override, server, group string) (Override, bool) {
	var best Override
	rank := -1
	for _, o := range overrides {
		r := -1
		switch {
		case o.Level == LevelCluster:
			r = 0
		case o.Level == LevelServerGroup && group != "" && o.Scope == group:
			r = 1
		case o.Level == LevelServer && o.Scope == server:
			r = 2
		}
		if r > rank {
			best, rank = o, r
		}
	}
	return best, rank >= 0
}

// codeNameReason refuses by name what PostgreSQL does not hide but what runs a command or loads code:
// a parameter of an extension that ends in _command, _library or _libraries, or preloads one. PostgreSQL 18
// shows archive_command and restore_command to every database user (checked), so they are not found by the
// superuser-only rule; their value is kept hidden by kingresd all the same (it may hold a secret).
func codeNameReason(name string) (instance.Reason, bool) {
	switch {
	case strings.HasSuffix(name, "_command"):
		return instance.Reason{Code: instance.ReasonRunsCommand, Text: "it runs a command as the operating system user of the database; set it on the server."}, true
	case strings.HasSuffix(name, "_library"), strings.HasSuffix(name, "_libraries"), strings.Contains(name, "preload"):
		return instance.Reason{Code: instance.ReasonLoadsCode, Text: "it loads code into the server; set it on the server."}, true
	}
	return instance.Reason{}, false
}
