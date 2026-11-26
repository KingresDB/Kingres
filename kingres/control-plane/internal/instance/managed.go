// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package instance

import "sort"

// ConfigFileName is the override file of an instance (design config-phase13.md): it holds the
// values the console set at the cluster, server group and server level. kingres-managed.conf
// includes it between its derived defaults and its fixed block, so an override beats a derived
// default and never a managed setting.
const ConfigFileName = "kingres-config.conf"

// Reason says why a parameter is not settable from the console.
type Reason struct {
	// Code is one of the codes below.
	Code string
	// Text is a sentence for the console.
	Text string
}

// Reason codes.
const (
	ReasonManaged         = "managed"
	ReasonRunsCommand     = "runsCommand"
	ReasonLoadsCode       = "loadsCode"
	ReasonNamesFile       = "namesFile"
	ReasonConnection      = "connectionString"
	ReasonTLS             = "tlsMaterial"
	ReasonLogging         = "logSetting"
	ReasonUnsafe          = "unsafe"
	ReasonDeveloper       = "developerOption"
	ReasonInternal        = "internal"
	ReasonSuperuserOnly   = "superuserOnly"
	ReasonSessionInternal = "sessionInternal"
	ReasonControlPlane    = "controlPlaneSession"
	ReasonStartRisk       = "startRisk"
)

func managed(text string) Reason { return Reason{ReasonManaged, text} }

func logging(text string) Reason { return Reason{ReasonLogging, text} }

// ManagedParameters are the parameters kingresd itself writes or relies on (design config-phase13.md,
// "Managed parameters"). It is the one list that the renderer, the standby code and the API share:
// RenderManaged writes its fixed block from FixedManaged, which this table must contain
// (TestManagedListIsComplete renders a file with every optional field set and fails on a name that
// is in neither), and the API refuses to set any of these names at any level.
var ManagedParameters = map[string]Reason{
	"listen_addresses":                  managed("kingresd sets the addresses the other servers and kingresd itself connect to; a wrong value cuts the server off."),
	"port":                              managed("kingresd sets the port the other servers and kingresd itself connect to."),
	"unix_socket_directories":           managed("kingresd connects to the instance through this socket."),
	"unix_socket_permissions":           managed("kingresd connects to the instance through this socket; a mode that closes it locks kingresd out."),
	"unix_socket_group":                 managed("kingresd connects to the instance through this socket; a group that does not exist stops the server from starting."),
	"log_destination":                   logging("the console reads the log files the collector writes; another destination leaves them empty."),
	"log_transaction_sample_rate":       managed("statement text must never be written to a log."),
	"log_statement_sample_rate":         managed("statement text must never be written to a log."),
	"log_min_duration_sample":           managed("statement text must never be written to a log."),
	"log_file_mode":                     logging("the console reads the log files, which need a mode its user can read."),
	"syslog_facility":                   logging("the logs go to the files the console reads."),
	"syslog_ident":                      logging("the logs go to the files the console reads."),
	"syslog_sequence_numbers":           logging("the logs go to the files the console reads."),
	"syslog_split_messages":             logging("the logs go to the files the console reads."),
	"shared_preload_libraries":          managed("the extensions of the cluster (kingres_shard, kingres_ledger, kingres_columnar, pg_stat_statements) are loaded through it."),
	"ssl":                               managed("kingresd sets the TLS files of the cluster certificates."),
	"ssl_cert_file":                     managed("the cluster certificate of this server."),
	"ssl_key_file":                      managed("the key of the cluster certificate of this server."),
	"ssl_ca_file":                       managed("the certificate authority of the cluster."),
	"ssl_crl_file":                      managed("the revocation list of the cluster."),
	"ssl_min_protocol_version":          managed("the cluster requires TLS 1.3."),
	"password_encryption":               managed("password verifiers are copied between servers and must be SCRAM."),
	"logging_collector":                 managed("the console reads the log files that the collector writes."),
	"log_directory":                     managed("set through the log directory of this server."),
	"log_filename":                      managed("the console reads the log files by this name."),
	"log_truncate_on_rotation":          managed("the console reads the log files by this rotation."),
	"log_min_error_statement":           managed("statement text must never be written to a log."),
	"log_statement":                     managed("statement text must never be written to a log."),
	"log_min_duration_statement":        managed("statement text must never be written to a log."),
	"log_parameter_max_length":          managed("parameter values must never be written to a log."),
	"log_parameter_max_length_on_error": managed("parameter values must never be written to a log."),
	"cluster_name":                      managed("kingresd names the process titles and logs of the instance."),
	"kingres_shard.node_conninfo":       managed("the certificate options of the connections between nodes."),
	"hot_standby":                       managed("a standby of a shard group is a hot standby."),
	"primary_conninfo":                  managed("the connection of a standby to its primary."),
	"primary_slot_name":                 managed("the replication slot of a standby."),
	"synchronous_standby_names":         managed("kingresd keeps it from the standbys that stream."),
	"synchronous_commit":                managed("the durability of each database sets it (ALTER DATABASE ... SET)."),
	"wal_level":                         managed("physical replication and the bucket moves of the sharding extension need it."),
	"hba_file":                          managed("pg_hba.conf is generated from the network rules."),
	"ident_file":                        managed("pg_ident.conf is generated."),
	"config_file":                       managed("the location of the configuration."),
	"data_directory":                    managed("the location of the data."),
	"external_pid_file":                 managed("the location of a process file."),
}

// DerivedDefaults are values kingresd chooses when it creates an instance (from the memory quota of
// the server and the connection count). They are written before the include of the override file, so
// an override wins over them; they are not managed.
var DerivedDefaults = map[string]bool{
	"max_connections": true, "max_prepared_transactions": true,
	"shared_buffers": true, "effective_cache_size": true, "maintenance_work_mem": true,
}

// ManagedNames returns the managed names in order.
func ManagedNames() []string {
	out := make([]string, 0, len(ManagedParameters))
	for k := range ManagedParameters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsManaged reports whether kingresd owns the parameter (name in lower case).
func IsManaged(name string) (Reason, bool) {
	r, ok := ManagedParameters[name]
	return r, ok
}

// MajorOfVersionNum returns the PostgreSQL major of a server_version_num ("180007" is 18,
// "190000" is 19). Behaviour that differs by major is decided from the number the running server
// reports, with this one function: no build tag per major and no version kingresd assumed.
func MajorOfVersionNum(num int) int { return num / 10000 }
