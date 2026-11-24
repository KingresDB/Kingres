// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co
//
// The parameters of the mock cluster: about 300 PostgreSQL-style settings with the metadata that `pg_settings` has
// (category, type, unit, range, enum values, context, built-in default, short description) plus the settings of the
// extensions `kingres_shard` and `kingres_ledger`. One line per parameter, because a table this long is easier to
// check against PostgreSQL's documentation than 300 object literals. The numbers are those of PostgreSQL 18 where the
// author remembers them and plausible otherwise; the mock only needs them to be consistent.
//
// Line format:  name | category | type | unit | min | max | boot | context | description
// `type` is b (bool), i (integer), r (real), s (string) or e (enum; the enum values are in the `min` column).
// The unit is the base unit of `pg_settings.unit` (`-` for none), `min` and `max` and `boot` are in that unit.
// `context` is p (postmaster), s (sighup), S (superuser), u (user), b (backend), B (superuser-backend), i (internal).

export const CATEGORY: Record<string, string> = {
  AV: 'Autovacuum',
  CL: 'Client Connection Defaults / Locale and Formatting',
  CO: 'Client Connection Defaults / Other Defaults',
  CP: 'Client Connection Defaults / Shared Library Preloading',
  CS: 'Client Connection Defaults / Statement Behavior',
  CA: 'Connections and Authentication / Authentication',
  CC: 'Connections and Authentication / Connection Settings',
  SS: 'Connections and Authentication / SSL',
  TC: 'Connections and Authentication / TCP Settings',
  XO: 'Customized Options',
  DV: 'Developer Options',
  EH: 'Error Handling',
  FL: 'File Locations',
  LM: 'Lock Management',
  PO: 'Preset Options',
  PT: 'Process Title',
  GQ: 'Query Tuning / Genetic Query Optimizer',
  QO: 'Query Tuning / Other Planner Options',
  PC: 'Query Tuning / Planner Cost Constants',
  PM: 'Query Tuning / Planner Method Configuration',
  RP: 'Replication / Primary Server',
  RS: 'Replication / Sending Servers',
  RY: 'Replication / Standby Servers',
  RB: 'Replication / Subscribers',
  LH: 'Reporting and Logging / When to Log',
  LR: 'Reporting and Logging / Where to Log',
  LW: 'Reporting and Logging / What to Log',
  RA: 'Resource Usage / Asynchronous Behavior',
  RW: 'Resource Usage / Background Writer',
  RV: 'Resource Usage / Cost-Based Vacuum Delay',
  RK: 'Resource Usage / Kernel Resources',
  RM: 'Resource Usage / Memory',
  SC: 'Statistics / Cumulative Query and Index Statistics',
  SM: 'Statistics / Monitoring',
  VC: 'Version and Platform Compatibility / Other Platforms and Clients',
  VP: 'Version and Platform Compatibility / Previous PostgreSQL Versions',
  WA: 'Write-Ahead Log / Archiving',
  WR: 'Write-Ahead Log / Archive Recovery',
  WC: 'Write-Ahead Log / Checkpoints',
  WF: 'Write-Ahead Log / Prefetching during Recovery',
  WY: 'Write-Ahead Log / Recovery',
  WT: 'Write-Ahead Log / Recovery Target',
  WS: 'Write-Ahead Log / Settings',
  WM: 'Write-Ahead Log / Summarization',
}

export const CATALOG = `
autovacuum|AV|b|-|||on|s|Starts the autovacuum subprocess.
autovacuum_analyze_scale_factor|AV|r|-|0|100|0.1|s|Number of tuple inserts, updates, or deletes prior to analyze as a fraction of reltuples.
autovacuum_analyze_threshold|AV|i|-|0|2147483647|50|s|Minimum number of tuple inserts, updates, or deletes prior to analyze.
autovacuum_freeze_max_age|AV|i|-|100000|2000000000|200000000|p|Age at which to autovacuum a table to prevent transaction ID wraparound.
autovacuum_max_workers|AV|i|-|1|262143|3|p|Sets the maximum number of simultaneously running autovacuum worker processes.
autovacuum_multixact_freeze_max_age|AV|i|-|10000|2000000000|400000000|p|Multixact age at which to autovacuum a table to prevent multixact wraparound.
autovacuum_naptime|AV|i|s|1|2147483|60|s|Time to sleep between autovacuum runs.
autovacuum_vacuum_cost_delay|AV|r|ms|-1|100|2|s|Vacuum cost delay in milliseconds, for autovacuum.
autovacuum_vacuum_cost_limit|AV|i|-|-1|10000|-1|s|Vacuum cost amount available before napping, for autovacuum.
autovacuum_vacuum_insert_scale_factor|AV|r|-|0|100|0.2|s|Number of tuple inserts prior to vacuum as a fraction of reltuples.
autovacuum_vacuum_insert_threshold|AV|i|-|-1|2147483647|1000|s|Minimum number of tuple inserts prior to vacuum, or -1 to disable insert vacuums.
autovacuum_vacuum_max_threshold|AV|i|-|-1|2147483647|100000000|s|Maximum number of tuple updates or deletes prior to vacuum.
autovacuum_vacuum_scale_factor|AV|r|-|0|100|0.2|s|Number of tuple updates or deletes prior to vacuum as a fraction of reltuples.
autovacuum_vacuum_threshold|AV|i|-|0|2147483647|50|s|Minimum number of tuple updates or deletes prior to vacuum.
autovacuum_work_mem|RM|i|kB|-1|2147483647|-1|s|Sets the maximum memory to be used by each autovacuum worker process.
autovacuum_worker_slots|AV|i|-|1|262143|16|p|Sets the number of backend slots to allocate for autovacuum workers.
log_autovacuum_min_duration|LW|i|ms|-1|2147483647|600000|s|Sets the minimum execution time above which autovacuum actions will be logged.
vacuum_buffer_usage_limit|RM|i|kB|0|16777216|2048|u|Sets the buffer pool size for VACUUM, ANALYZE, and autovacuum.
vacuum_cost_delay|RV|r|ms|0|100|0|u|Vacuum cost delay in milliseconds.
vacuum_cost_limit|RV|i|-|1|10000|200|u|Vacuum cost amount available before napping.
vacuum_cost_page_dirty|RV|i|-|0|10000|20|u|Vacuum cost for a page dirtied by vacuum.
vacuum_cost_page_hit|RV|i|-|0|10000|1|u|Vacuum cost for a page found in the buffer cache.
vacuum_cost_page_miss|RV|i|-|0|10000|2|u|Vacuum cost for a page not found in the buffer cache.
vacuum_failsafe_age|CS|i|-|0|2100000000|1600000000|u|Age at which VACUUM should trigger failsafe to avoid a wraparound outage.
vacuum_freeze_min_age|CS|i|-|0|1000000000|50000000|u|Minimum age at which VACUUM should freeze a table row.
vacuum_freeze_table_age|CS|i|-|0|2000000000|150000000|u|Age at which VACUUM should scan whole table to freeze tuples.
vacuum_max_eager_freeze_failure_rate|CS|r|-|0|1|0.03|u|Fraction of pages in a relation vacuum can scan and fail to freeze before disabling eager scanning.
vacuum_multixact_failsafe_age|CS|i|-|0|2100000000|1600000000|u|Multixact age at which VACUUM should trigger failsafe to avoid a wraparound outage.
vacuum_multixact_freeze_min_age|CS|i|-|0|1000000000|5000000|u|Minimum age at which VACUUM should freeze a MultiXactId in a table row.
vacuum_multixact_freeze_table_age|CS|i|-|0|2000000000|150000000|u|Multixact age at which VACUUM should scan whole table to freeze tuples.
vacuum_truncate|CS|b|-|||on|u|Enables vacuum to truncate empty pages at the end of the table.
DateStyle|CL|s|-|||ISO, MDY|u|Sets the display format for date and time values.
IntervalStyle|CL|e|-|postgres,postgres_verbose,sql_standard,iso_8601||postgres|u|Sets the display format for interval values.
TimeZone|CL|s|-|||GMT|u|Sets the time zone for displaying and interpreting time stamps.
bytea_output|CL|e|-|hex,escape||hex|u|Sets the output format for bytea.
client_encoding|CL|s|-|||SQL_ASCII|u|Sets the client's character set encoding.
default_text_search_config|CL|s|-|||pg_catalog.simple|u|Sets default text search configuration.
extra_float_digits|CL|i|-|-15|3|1|u|Sets the number of digits displayed for floating-point values.
icu_validation_level|CL|e|-|disabled,debug5,debug4,debug3,debug2,debug1,log,notice,warning,error||warning|u|Log level for reporting invalid ICU locale strings.
lc_messages|CL|s|-|||C|S|Sets the language in which messages are displayed.
lc_monetary|CL|s|-|||C|u|Sets the locale for formatting monetary amounts.
lc_numeric|CL|s|-|||C|u|Sets the locale for formatting numbers.
lc_time|CL|s|-|||C|u|Sets the locale for formatting date and time values.
log_timezone|LR|s|-|||GMT|s|Sets the time zone to use in log messages.
timezone_abbreviations|CL|s|-|||Default|u|Selects a file of time zone abbreviations.
check_function_bodies|CS|b|-|||on|u|Check routine bodies during CREATE FUNCTION and CREATE PROCEDURE.
default_table_access_method|CS|s|-|||heap|u|Sets the default table access method for new tables.
default_tablespace|CS|s|-|||| u|Sets the default tablespace to create tables and indexes in.
default_toast_compression|CS|e|-|pglz,lz4||pglz|u|Sets the default compression method for compressible values.
default_transaction_deferrable|CS|b|-|||off|u|Sets the default deferrable status of new transactions.
default_transaction_isolation|CS|e|-|serializable,repeatable read,read committed,read uncommitted||read committed|u|Sets the transaction isolation level of each new transaction.
default_transaction_read_only|CS|b|-|||off|u|Sets the default read-only status of new transactions.
gin_pending_list_limit|CO|i|kB|64|2147483647|4096|u|Sets the maximum size of the pending list for GIN index.
idle_in_transaction_session_timeout|CS|i|ms|0|2147483647|0|u|Sets the maximum allowed idle time between queries, when in a transaction.
idle_session_timeout|CS|i|ms|0|2147483647|0|u|Sets the maximum allowed idle time between queries, when not in a transaction.
lock_timeout|CS|i|ms|0|2147483647|0|u|Sets the maximum allowed duration of any wait for a lock.
row_security|CS|b|-|||on|u|Enable row security.
search_path|CS|s|-|||"$user", public|u|Sets the schema search order for names that are not schema-qualified.
session_replication_role|CS|e|-|origin,replica,local||origin|S|Sets the session's behavior for triggers and rewrite rules.
statement_timeout|CS|i|ms|0|2147483647|0|u|Sets the maximum allowed duration of any statement.
transaction_timeout|CS|i|ms|0|2147483647|0|u|Sets the maximum allowed duration of any transaction within a session.
xmlbinary|CO|e|-|base64,hex||base64|u|Sets how binary values are to be encoded in XML.
xmloption|CO|e|-|content,document||content|u|Sets whether XML data in implicit parsing and serialization operations is to be considered as documents or content fragments.
dynamic_library_path|CO|s|-|||$libdir|S|Sets the path for dynamically loadable modules.
extension_control_path|CO|s|-|||$system|S|Sets the path for extension control files.
jit|QO|b|-|||on|u|Allow JIT compilation.
jit_above_cost|PC|r|-|-1|1.79769e+308|100000|u|Perform JIT compilation if query is more expensive.
jit_expressions|DV|b|-|||on|u|Allow JIT compilation of expressions.
jit_inline_above_cost|PC|r|-|-1|1.79769e+308|500000|u|Perform JIT inlining if query is more expensive.
jit_optimize_above_cost|PC|r|-|-1|1.79769e+308|500000|u|Optimize JIT-compiled functions if query is more expensive.
jit_provider|CP|s|-|||llvmjit|p|JIT provider to use.
jit_tuple_deconstruction|DV|b|-|||on|u|Allow JIT compilation of tuple deconstruction.
local_preload_libraries|CP|s|-||||B|Lists unprivileged shared libraries to preload into each backend.
session_preload_libraries|CP|s|-||||S|Lists shared libraries to preload into each backend.
shared_preload_libraries|CP|s|-|||kingres_shard,kingres_ledger,kingres_columnar,pg_stat_statements|p|Lists shared libraries to preload into server.
oauth_validator_libraries|CA|s|-||||s|Lists libraries that may be called to validate OAuth v2 bearer tokens.
authentication_timeout|CA|i|s|1|600|60|s|Sets the maximum allowed time to complete client authentication.
password_encryption|CA|e|-|md5,scram-sha-256||scram-sha-256|u|Chooses the algorithm for encrypting passwords.
scram_iterations|CA|i|-|1|2147483647|4096|u|Sets the iteration count for SCRAM secret generation.
krb_caseins_users|CA|b|-|||off|s|Sets whether Kerberos and GSSAPI user names should be treated as case-insensitive.
krb_server_keyfile|CA|s|-|||FILE:/etc/postgresql-common/krb5.keytab|s|Sets the location of the Kerberos server key file.
bonjour|CC|b|-|||off|p|Enables advertising the server via Bonjour.
bonjour_name|CC|s|-||||p|Sets the Bonjour service name.
listen_addresses|CC|s|-|||localhost|p|Sets the host name or IP address(es) to listen to.
max_connections|CC|i|-|1|262143|100|p|Sets the maximum number of concurrent connections.
max_prepared_transactions|RM|i|-|0|262143|0|p|Sets the maximum number of simultaneously prepared transactions.
max_notify_queue_pages|RM|i|-|64|2147483647|1048576|p|Sets the maximum number of allocated pages for NOTIFY / LISTEN queue.
port|CC|i|-|1|65535|5432|p|Sets the TCP port the server listens on.
reserved_connections|CC|i|-|0|262143|0|p|Sets the number of connection slots reserved for roles with privileges of pg_use_reserved_connections.
superuser_reserved_connections|CC|i|-|0|262143|3|p|Sets the number of connection slots reserved for superusers.
unix_socket_directories|CC|s|-|||/var/run/postgresql|p|Sets the directories where Unix-domain sockets will be created.
unix_socket_group|CC|s|-||||p|Sets the owning group of the Unix-domain socket.
unix_socket_permissions|CC|i|-|0|511|511|p|Sets the access permissions of the Unix-domain socket.
client_connection_check_interval|CC|i|ms|0|2147483647|0|u|Sets the time interval between checks for disconnection while running queries.
tcp_keepalives_count|TC|i|-|0|2147483647|0|u|Maximum number of TCP keepalive retransmits.
tcp_keepalives_idle|TC|i|s|0|2147483647|0|u|Time between issuing TCP keepalives.
tcp_keepalives_interval|TC|i|s|0|2147483647|0|u|Time between TCP keepalive retransmits.
tcp_user_timeout|TC|i|ms|0|2147483647|0|u|TCP user timeout.
ssl|SS|b|-|||off|s|Enables SSL connections.
ssl_ca_file|SS|s|-||||s|Location of the SSL certificate authority file.
ssl_cert_file|SS|s|-|||server.crt|s|Location of the SSL server certificate file.
ssl_ciphers|SS|s|-|||HIGH:MEDIUM:+3DES:!aNULL|s|Sets the list of allowed TLSv1.2 (and lower) ciphers.
ssl_crl_dir|SS|s|-||||s|Location of the SSL certificate revocation list directory.
ssl_crl_file|SS|s|-||||s|Location of the SSL certificate revocation list file.
ssl_dh_params_file|SS|s|-||||s|Location of the SSL DH parameters file.
ssl_ecdh_curve|SS|s|-|||prime256v1|s|Sets the curve to use for ECDH.
ssl_groups|SS|s|-|||X25519:prime256v1|s|Sets the group(s) to use for Diffie-Hellman key exchange.
ssl_key_file|SS|s|-|||server.key|s|Location of the SSL server private key file.
ssl_max_protocol_version|SS|e|-|,TLSv1,TLSv1.1,TLSv1.2,TLSv1.3|||s|Sets the maximum SSL/TLS protocol version to use.
ssl_min_protocol_version|SS|e|-|TLSv1,TLSv1.1,TLSv1.2,TLSv1.3||TLSv1.2|s|Sets the minimum SSL/TLS protocol version to use.
ssl_passphrase_command|SS|s|-||||s|Command to obtain passphrases for SSL.
ssl_passphrase_command_supports_reload|SS|b|-|||off|s|Controls whether ssl_passphrase_command is called during server reload.
ssl_prefer_server_ciphers|SS|b|-|||on|s|Give priority to server ciphersuite order.
ssl_tls13_ciphers|SS|s|-||||s|Sets the list of allowed TLSv1.3 ciphersuites.
allow_in_place_tablespaces|DV|b|-|||off|S|Allows tablespaces directly inside pg_tblspc, for testing.
allow_system_table_mods|DV|b|-|||off|S|Allows modifications of the structure of system tables.
backtrace_functions|DV|s|-||||S|Log backtrace for errors in these functions.
debug_discard_caches|DV|i|-|0|0|0|S|Aggressively flush system caches for debugging purposes.
debug_io_direct|DV|s|-||||p|Use direct I/O for file access.
debug_parallel_query|DV|e|-|off,on,regress||off|u|Forces the planner's use parallel query nodes.
ignore_checksum_failure|DV|b|-|||off|S|Continues processing after a checksum failure.
ignore_invalid_pages|DV|b|-|||off|p|Continues recovery after an invalid pages failure.
post_auth_delay|DV|i|s|0|2147|0|B|Sets the amount of time to wait after authentication on connection startup.
pre_auth_delay|DV|i|s|0|60|0|s|Sets the amount of time to wait before authentication on connection startup.
trace_notify|DV|b|-|||off|u|Generates debugging output for LISTEN and NOTIFY.
trace_sort|DV|b|-|||off|u|Emit information about resource usage in sorting.
wal_consistency_checking|DV|s|-||||S|Sets the WAL resource managers for which WAL consistency checks are done.
zero_damaged_pages|DV|b|-|||off|S|Continues processing past damaged page headers.
exit_on_error|EH|b|-|||off|u|Terminate session on any error.
restart_after_crash|EH|b|-|||on|s|Reinitialize server after backend crash.
recovery_init_sync_method|EH|e|-|fsync,syncfs||fsync|p|Sets the method for synchronizing the data directory before crash recovery.
data_sync_retry|EH|b|-|||off|p|Whether to continue running after a failure to sync data files.
config_file|FL|s|-||||p|Sets the server's main configuration file.
data_directory|FL|s|-||||p|Sets the server's data directory.
external_pid_file|FL|s|-||||p|Writes the postmaster PID to the specified file.
hba_file|FL|s|-||||p|Sets the server's "hba" configuration file.
ident_file|FL|s|-||||p|Sets the server's "ident" configuration file.
deadlock_timeout|LM|i|ms|1|2147483647|1000|S|Sets the time to wait on a lock before checking for deadlock.
max_locks_per_transaction|LM|i|-|10|2147483647|64|p|Sets the maximum number of locks per transaction.
max_pred_locks_per_page|LM|i|-|0|2147483647|2|s|Sets the maximum number of predicate-locked tuples per page.
max_pred_locks_per_relation|LM|i|-|-2147483648|2147483647|-2|s|Sets the maximum number of predicate-locked pages and tuples per relation.
max_pred_locks_per_transaction|LM|i|-|10|2147483647|64|p|Sets the maximum number of predicate locks per transaction.
block_size|PO|i|-|8192|8192|8192|i|Shows the size of a disk block.
data_checksums|PO|b|-|||off|i|Shows whether data checksums are turned on for this cluster.
data_directory_mode|PO|i|-|0|511|448|i|Shows the mode of the data directory.
debug_assertions|PO|b|-|||off|i|Shows whether the running server has assertion checks enabled.
in_hot_standby|PO|b|-|||off|i|Shows whether hot standby is currently active.
integer_datetimes|PO|b|-|||on|i|Shows whether datetimes are integer based.
max_function_args|PO|i|-|100|100|100|i|Shows the maximum number of function arguments.
max_identifier_length|PO|i|-|63|63|63|i|Shows the maximum identifier length.
max_index_keys|PO|i|-|32|32|32|i|Shows the maximum number of index keys.
segment_size|PO|i|8kB|131072|131072|131072|i|Shows the number of pages per disk file.
server_encoding|PO|s|-|||UTF8|i|Shows the server (database) character set encoding.
server_version|PO|s|-|||18.7|i|Shows the server version.
server_version_num|PO|i|-|180007|180007|180007|i|Shows the server version as an integer.
shared_memory_size|PO|i|MB|0|2147483647|148|i|Shows the size of the server's main shared memory area (rounded up to the nearest MB).
ssl_library|PO|s|-|||OpenSSL|i|Shows the name of the SSL library.
wal_block_size|PO|i|-|8192|8192|8192|i|Shows the block size in the write ahead log.
wal_segment_size|PO|i|B|1048576|1073741824|16777216|i|Shows the size of write ahead log segments.
cluster_name|PT|s|-||||p|Sets the name of the cluster, which is included in the process title.
update_process_title|PT|b|-|||on|S|Updates the process title to show the active SQL command.
cpu_index_tuple_cost|PC|r|-|0|1.79769e+308|0.005|u|Sets the planner's estimate of the cost of processing each index entry during an index scan.
cpu_operator_cost|PC|r|-|0|1.79769e+308|0.0025|u|Sets the planner's estimate of the cost of processing each operator or function call.
cpu_tuple_cost|PC|r|-|0|1.79769e+308|0.01|u|Sets the planner's estimate of the cost of processing each tuple (row).
effective_cache_size|PC|i|8kB|1|2147483647|524288|u|Sets the planner's assumption about the total size of the data caches.
min_parallel_index_scan_size|PC|i|8kB|0|715827882|64|u|Sets the minimum amount of index data for a parallel scan.
min_parallel_table_scan_size|PC|i|8kB|0|715827882|1024|u|Sets the minimum amount of table data for a parallel scan.
parallel_setup_cost|PC|r|-|0|1.79769e+308|1000|u|Sets the planner's estimate of the cost of starting up worker processes for parallel query.
parallel_tuple_cost|PC|r|-|0|1.79769e+308|0.1|u|Sets the planner's estimate of the cost of passing each tuple (row) from worker to leader backend.
random_page_cost|PC|r|-|0|1.79769e+308|4|u|Sets the planner's estimate of the cost of a nonsequentially fetched disk page.
seq_page_cost|PC|r|-|0|1.79769e+308|1|u|Sets the planner's estimate of the cost of a sequentially fetched disk page.
cursor_tuple_fraction|QO|r|-|0|1|0.1|u|Sets the planner's estimate of the fraction of a cursor's rows that will be retrieved.
default_statistics_target|QO|i|-|1|10000|100|u|Sets the default statistics target.
from_collapse_limit|QO|i|-|1|2147483647|8|u|Sets the FROM-list size beyond which subqueries are not collapsed.
join_collapse_limit|QO|i|-|1|2147483647|8|u|Sets the FROM-list size beyond which JOIN constructs are not flattened.
plan_cache_mode|QO|e|-|auto,force_generic_plan,force_custom_plan||auto|u|Controls the planner's selection of custom or generic plan.
recursive_worktable_factor|QO|r|-|0.001|1000000|10|u|Sets the planner's estimate of the average size of a recursive query's working table.
constraint_exclusion|QO|e|-|partition,on,off||partition|u|Enables the planner to use constraints to optimize queries.
geqo|GQ|b|-|||on|u|Enables genetic query optimization.
geqo_effort|GQ|i|-|1|10|5|u|GEQO: effort is used to set the default for other GEQO parameters.
geqo_generations|GQ|i|-|0|2147483647|0|u|GEQO: number of iterations of the algorithm.
geqo_pool_size|GQ|i|-|0|2147483647|0|u|GEQO: number of individuals in the population.
geqo_seed|GQ|r|-|0|1|0|u|GEQO: seed for random path selection.
geqo_selection_bias|GQ|r|-|1.5|2|2|u|GEQO: selective pressure within the population.
geqo_threshold|GQ|i|-|2|2147483647|12|u|Sets the threshold of FROM items beyond which GEQO is used.
enable_async_append|PM|b|-|||on|u|Enables the planner's use of async append plans.
enable_bitmapscan|PM|b|-|||on|u|Enables the planner's use of bitmap-scan plans.
enable_distinct_reordering|PM|b|-|||on|u|Enables reordering of DISTINCT keys.
enable_gathermerge|PM|b|-|||on|u|Enables the planner's use of gather merge plans.
enable_group_by_reordering|PM|b|-|||on|u|Enables reordering of GROUP BY keys.
enable_hashagg|PM|b|-|||on|u|Enables the planner's use of hashed aggregation plans.
enable_hashjoin|PM|b|-|||on|u|Enables the planner's use of hash join plans.
enable_incremental_sort|PM|b|-|||on|u|Enables the planner's use of incremental sort steps.
enable_indexonlyscan|PM|b|-|||on|u|Enables the planner's use of index-only-scan plans.
enable_indexscan|PM|b|-|||on|u|Enables the planner's use of index-scan plans.
enable_material|PM|b|-|||on|u|Enables the planner's use of materialization.
enable_memoize|PM|b|-|||on|u|Enables the planner's use of memoization.
enable_mergejoin|PM|b|-|||on|u|Enables the planner's use of merge join plans.
enable_nestloop|PM|b|-|||on|u|Enables the planner's use of nested-loop join plans.
enable_parallel_append|PM|b|-|||on|u|Enables the planner's use of parallel append plans.
enable_parallel_hash|PM|b|-|||on|u|Enables the planner's use of parallel hash plans.
enable_partition_pruning|PM|b|-|||on|u|Enables plan-time and execution-time partition pruning.
enable_partitionwise_aggregate|PM|b|-|||off|u|Enables partitionwise aggregation and grouping.
enable_partitionwise_join|PM|b|-|||off|u|Enables partitionwise join.
enable_presorted_aggregate|PM|b|-|||on|u|Enables the planner's ability to produce plans that provide presorted input for ORDER BY / DISTINCT aggregate functions.
enable_self_join_elimination|PM|b|-|||on|u|Enables removal of unique self-joins.
enable_seqscan|PM|b|-|||on|u|Enables the planner's use of sequential-scan plans.
enable_sort|PM|b|-|||on|u|Enables the planner's use of explicit sort steps.
enable_tidscan|PM|b|-|||on|u|Enables the planner's use of TID scan plans.
hot_standby|RY|b|-|||on|p|Allows connections and queries during recovery.
hot_standby_feedback|RY|b|-|||off|s|Allows feedback from a hot standby to the primary that will avoid query conflicts.
max_standby_archive_delay|RY|i|ms|-1|2147483647|30000|s|Sets the maximum delay before canceling queries when a hot standby server is processing archived WAL data.
max_standby_streaming_delay|RY|i|ms|-1|2147483647|30000|s|Sets the maximum delay before canceling queries when a hot standby server is processing streamed WAL data.
primary_conninfo|RY|s|-||||s|Sets the connection string to be used to connect to the sending server.
primary_slot_name|RY|s|-||||s|Sets the name of the replication slot to use on the sending server.
recovery_min_apply_delay|RY|i|ms|0|2147483647|0|s|Sets the minimum delay for applying changes during recovery.
sync_replication_slots|RY|b|-|||off|s|Enables a physical standby to synchronize logical failover replication slots from the primary server.
synchronized_standby_slots|RP|s|-||||s|Lists streaming replication standby server replication slot names that logical WAL sender processes will wait for.
wal_receiver_create_temp_slot|RY|b|-|||off|s|Sets whether a WAL receiver should create a temporary replication slot if no permanent slot is configured.
wal_receiver_status_interval|RY|i|s|0|2147483|10|s|Sets the maximum interval between WAL receiver status reports to the sending server.
wal_receiver_timeout|RY|i|ms|0|2147483647|60000|s|Sets the maximum wait time to receive data from the sending server.
wal_retrieve_retry_interval|RY|i|ms|1|2147483647|5000|s|Sets the time to wait before retrying to retrieve WAL after a failed attempt.
idle_replication_slot_timeout|RS|i|s|0|2147483647|0|s|Sets the duration a replication slot can remain idle before it is invalidated.
max_replication_slots|RS|i|-|0|262143|10|p|Sets the maximum number of simultaneously defined replication slots.
max_slot_wal_keep_size|RS|i|MB|-1|2147483647|-1|s|Sets the maximum WAL size that can be reserved by replication slots.
max_wal_senders|RS|i|-|0|262143|10|p|Sets the maximum number of simultaneously running WAL sender processes.
track_commit_timestamp|RS|b|-|||off|p|Collects transaction commit time.
wal_keep_size|RS|i|MB|0|2147483647|0|s|Sets the size of WAL files held for standby servers.
wal_sender_timeout|RS|i|ms|0|2147483647|60000|s|Sets the maximum time to wait for WAL replication.
synchronous_standby_names|RP|s|-||||s|Number of synchronous standbys and list of names of potential synchronous ones.
synchronous_commit|WS|e|-|local,remote_write,remote_apply,on,off||on|u|Sets the current transaction's synchronization level.
max_logical_replication_workers|RB|i|-|0|262143|4|p|Maximum number of logical replication worker processes.
max_sync_workers_per_subscription|RB|i|-|0|262143|2|s|Maximum number of table synchronization workers per subscription.
max_parallel_apply_workers_per_subscription|RB|i|-|0|1024|2|s|Maximum number of parallel apply workers per subscription.
log_checkpoints|LW|b|-|||on|s|Logs each checkpoint.
log_connections|LW|s|-||||B|Logs each successful connection.
log_disconnections|LW|b|-|||off|B|Logs end of a session, including duration.
log_duration|LW|b|-|||off|S|Logs the duration of each completed SQL statement.
log_error_verbosity|LW|e|-|terse,default,verbose||default|S|Sets the verbosity of logged messages.
log_hostname|LW|b|-|||off|s|Logs the host name in the connection logs.
log_line_prefix|LW|s|-|||%m [%p] |s|Controls information prefixed to each log line.
log_lock_failures|LW|b|-|||off|S|Logs lock failures.
log_lock_waits|LW|b|-|||off|S|Logs long lock waits.
log_parameter_max_length|LW|i|B|-1|1073741823|-1|S|Sets the maximum length in bytes of data logged for bind parameter values when logging statements.
log_parameter_max_length_on_error|LW|i|B|-1|1073741823|0|u|Sets the maximum length in bytes of data logged for bind parameter values when logging statements, on error.
log_recovery_conflict_waits|LW|b|-|||off|s|Logs standby recovery conflict waits.
log_replication_commands|LW|b|-|||off|S|Logs each replication command.
log_statement|LW|e|-|none,ddl,mod,all||none|S|Sets the type of statements logged.
log_statement_sample_rate|LH|r|-|0|1|1|S|Fraction of statements exceeding log_min_duration_sample to be logged.
log_temp_files|LW|i|kB|-1|2147483647|-1|S|Log the use of temporary files larger than this number of kilobytes.
log_transaction_sample_rate|LH|r|-|0|1|0|S|Sets the fraction of transactions from which to log all statements.
log_min_duration_sample|LH|i|ms|-1|2147483647|-1|S|Sets the minimum execution time above which a sample of statements will be logged.
log_min_duration_statement|LH|i|ms|-1|2147483647|-1|S|Sets the minimum execution time above which all statements will be logged.
log_min_error_statement|LH|e|-|debug5,debug4,debug3,debug2,debug1,info,notice,warning,error,log,fatal,panic||error|S|Causes all statements generating error at or above this level to be logged.
log_min_messages|LH|e|-|debug5,debug4,debug3,debug2,debug1,info,notice,warning,error,log,fatal,panic||warning|S|Sets the message levels that are logged.
log_startup_progress_interval|LH|i|ms|0|2147483647|10000|s|Time between progress updates for long-running startup operations.
logging_collector|LR|b|-|||off|p|Start a subprocess to capture stderr, csvlog and/or jsonlog output into log files.
log_destination|LR|s|-|||stderr|s|Sets the destination for server log output.
log_directory|LR|s|-|||log|s|Sets the destination directory for log files.
log_file_mode|LR|i|-|0|511|384|s|Sets the file permissions for log files.
log_filename|LR|s|-|||postgresql-%Y-%m-%d_%H%M%S.log|s|Sets the file name pattern for log files.
log_rotation_age|LR|i|min|0|35791394|1440|s|Sets the amount of time to wait before forcing log file rotation.
log_rotation_size|LR|i|kB|0|2097151|10240|s|Sets the maximum size a log file can reach before being rotated.
log_truncate_on_rotation|LR|b|-|||off|s|Truncate existing log files of same name during log rotation.
syslog_facility|LR|e|-|local0,local1,local2,local3,local4,local5,local6,local7||local0|s|Sets the syslog "facility" to be used when syslog enabled.
syslog_ident|LR|s|-|||postgres|s|Sets the program name used to identify PostgreSQL messages in syslog.
syslog_sequence_numbers|LR|b|-|||on|s|Add sequence number to syslog messages to avoid duplicate suppression.
syslog_split_messages|LR|b|-|||on|s|Split messages sent to syslog by lines and to fit into 1024 bytes.
event_source|LR|s|-|||PostgreSQL|p|Sets the application name used to identify PostgreSQL messages in the event log.
backend_flush_after|RA|i|8kB|0|256|0|u|Number of pages after which previously performed writes are flushed to disk.
effective_io_concurrency|RA|i|-|0|1000|16|u|Number of simultaneous requests that can be handled efficiently by the disk subsystem.
io_combine_limit|RA|i|8kB|1|128|16|u|Limit on the size of data reads and writes.
io_max_combine_limit|RA|i|8kB|1|128|16|p|Server-wide limit that clamps io_combine_limit.
io_max_concurrency|RA|i|-|-1|1024|-1|p|Max number of IOs that one process can execute simultaneously.
io_method|RA|e|-|worker,io_uring,sync||worker|p|Selects the method for executing asynchronous I/O.
io_workers|RA|i|-|1|32|3|s|Number of IO worker processes, for io_method=worker.
maintenance_io_concurrency|RA|i|-|0|1000|16|u|A variant of effective_io_concurrency that is used for maintenance work.
max_parallel_maintenance_workers|RA|i|-|0|1024|2|u|Sets the maximum number of parallel processes per maintenance operation.
max_parallel_workers|RA|i|-|0|1024|8|u|Sets the maximum number of parallel workers that can be active at one time.
max_parallel_workers_per_gather|RA|i|-|0|1024|2|u|Sets the maximum number of parallel processes per executor node.
max_worker_processes|RA|i|-|0|262143|8|p|Maximum number of concurrent worker processes.
parallel_leader_participation|RA|b|-|||on|u|Controls whether Gather and Gather Merge also run subplans.
bgwriter_delay|RW|i|ms|10|10000|200|s|Background writer sleep time between rounds.
bgwriter_flush_after|RW|i|8kB|0|256|64|s|Number of pages after which previously performed writes are flushed to disk.
bgwriter_lru_maxpages|RW|i|-|0|1073741823|100|s|Background writer maximum number of LRU pages to flush per round.
bgwriter_lru_multiplier|RW|r|-|0|10|2|s|Multiple of the average buffer usage to free per round.
file_copy_method|RK|e|-|copy,clone||copy|u|Selects the file copy method.
max_files_per_process|RK|i|-|64|2147483647|1000|p|Sets the maximum number of files each server process is allowed to open simultaneously.
huge_page_size|RM|i|kB|0|2147483647|0|p|The size of huge page that should be requested.
huge_pages|RM|e|-|off,on,try||try|p|Use of huge pages on Linux or Windows.
hash_mem_multiplier|RM|r|-|1|1000|2|u|Multiple of work_mem to use for hash tables.
logical_decoding_work_mem|RM|i|kB|64|2147483647|65536|u|Sets the maximum memory to be used for logical decoding.
maintenance_work_mem|RM|i|kB|64|2147483647|65536|u|Sets the maximum memory to be used for maintenance operations.
min_dynamic_shared_memory|RM|i|MB|0|715827882|0|p|Amount of dynamic shared memory reserved at startup.
shared_buffers|RM|i|8kB|16|1073741823|16384|p|Sets the number of shared memory buffers used by the server.
shared_memory_type|RM|e|-|mmap,sysv,windows||mmap|p|Selects the shared memory implementation used for the main shared memory region.
temp_buffers|RM|i|8kB|100|1073741823|1024|u|Sets the maximum number of temporary buffers used by each session.
temp_file_limit|RK|i|kB|-1|2147483647|-1|S|Limits the total size of all temporary files used by each process.
work_mem|RM|i|kB|64|2147483647|4096|u|Sets the maximum memory to be used for query workspaces.
dynamic_shared_memory_type|RM|e|-|posix,sysv,mmap||posix|p|Selects the dynamic shared memory implementation used.
max_stack_depth|RM|i|kB|100|2147483647|2048|S|Sets the maximum stack depth, in kilobytes.
track_activities|SC|b|-|||on|S|Collects information about executing commands.
track_activity_query_size|SC|i|B|100|1048576|1024|p|Sets the size reserved for pg_stat_activity.query, in bytes.
track_cost_delay_timing|SC|b|-|||off|S|Collects timing statistics for cost-based vacuum delay.
track_counts|SC|b|-|||on|S|Collects statistics on database activity.
track_functions|SC|e|-|none,pl,all||none|S|Collects function-level statistics on database activity.
track_io_timing|SC|b|-|||off|S|Collects timing statistics for database I/O activity.
track_wal_io_timing|SC|b|-|||off|S|Collects timing statistics for WAL I/O activity.
stats_fetch_consistency|SC|e|-|none,cache,snapshot||cache|u|Sets the consistency of accesses to statistics data.
compute_query_id|SM|e|-|off,on,auto,regress||auto|S|Enables in-core computation of query identifiers.
log_executor_stats|SM|b|-|||off|S|Writes executor performance statistics to the server log.
log_parser_stats|SM|b|-|||off|S|Writes parser performance statistics to the server log.
log_planner_stats|SM|b|-|||off|S|Writes planner performance statistics to the server log.
log_statement_stats|SM|b|-|||off|S|Writes cumulative performance statistics to the server log.
array_nulls|VC|b|-|||on|u|Enable input of NULL elements in arrays.
backslash_quote|VC|e|-|safe_encoding,on,off||safe_encoding|u|Sets whether "\\'" is allowed in string literals.
escape_string_warning|VC|b|-|||on|u|Warn about backslash escapes in ordinary string literals.
lo_compat_privileges|VC|b|-|||off|S|Enables backward compatibility mode for privilege checks on large objects.
quote_all_identifiers|VC|b|-|||off|u|When generating SQL fragments, quote all identifiers.
standard_conforming_strings|VC|b|-|||on|u|Causes '...' strings to treat backslashes literally.
synchronize_seqscans|VC|b|-|||on|u|Enable synchronized sequential scans.
transform_null_equals|VP|b|-|||off|u|Treats "expr=NULL" as "expr IS NULL".
archive_command|WA|s|-||||s|Sets the shell command that will be called to archive a WAL file.
archive_library|WA|s|-||||s|Sets the library that will be called to archive a WAL file.
archive_mode|WA|e|-|always,on,off||off|p|Allows archiving of WAL files using archive_command.
archive_timeout|WA|i|s|0|1073741823|0|s|Sets the amount of time to wait before forcing a switch to the next WAL file.
archive_cleanup_command|WR|s|-||||s|Sets the shell command that will be executed at every restart point.
recovery_end_command|WR|s|-||||s|Sets the shell command that will be executed once at the end of recovery.
restore_command|WR|s|-||||s|Sets the shell command that will be called to retrieve an archived WAL file.
checkpoint_completion_target|WC|r|-|0|1|0.9|s|Time spent flushing dirty buffers during checkpoint, as fraction of checkpoint interval.
checkpoint_flush_after|WC|i|8kB|0|256|32|s|Number of pages after which previously performed writes are flushed to disk.
checkpoint_timeout|WC|i|s|30|86400|300|s|Sets the maximum time between automatic WAL checkpoints.
checkpoint_warning|WC|i|s|0|2147483647|30|s|Sets the maximum time before warning if checkpoints triggered by WAL volume happen too frequently.
max_wal_size|WC|i|MB|2|2147483647|1024|s|Sets the WAL size that triggers a checkpoint.
min_wal_size|WC|i|MB|2|2147483647|80|s|Sets the minimum size to shrink the WAL to.
recovery_prefetch|WF|e|-|off,on,try||try|s|Prefetch referenced blocks during recovery.
wal_decode_buffer_size|WF|i|B|524288|1073741823|524288|p|Buffer size for reading ahead in the WAL during recovery.
recovery_target|WT|e|-|immediate|||p|Set to "immediate" to end recovery as soon as a consistent state is reached.
recovery_target_action|WT|e|-|pause,promote,shutdown||pause|p|Sets the action to perform upon reaching the recovery target.
recovery_target_inclusive|WT|b|-|||on|p|Sets whether to include or exclude transaction with recovery target.
recovery_target_lsn|WT|s|-||||p|Sets the LSN of the write-ahead log location up to which recovery will proceed.
recovery_target_name|WT|s|-||||p|Sets the named restore point up to which recovery will proceed.
recovery_target_time|WT|s|-||||p|Sets the time stamp up to which recovery will proceed.
recovery_target_timeline|WT|s|-|||latest|p|Specifies the timeline to recover into.
recovery_target_xid|WT|s|-||||p|Sets the transaction ID up to which recovery will proceed.
commit_delay|WS|i|-|0|100000|0|S|Sets the delay in microseconds between transaction commit and flushing WAL to disk.
commit_siblings|WS|i|-|0|1000|5|u|Sets the minimum number of concurrent open transactions required before performing commit_delay.
fsync|WS|b|-|||on|s|Forces synchronization of updates to disk.
full_page_writes|WS|b|-|||on|s|Writes full pages to WAL when first modified after a checkpoint.
wal_buffers|WS|i|8kB|-1|262143|-1|p|Sets the number of disk-page buffers in shared memory for WAL.
wal_compression|WS|e|-|pglz,lz4,zstd,on,off||off|S|Compresses full-page writes written in WAL file with specified method.
wal_init_zero|WS|b|-|||on|s|Writes zeroes to new WAL files before first use.
wal_level|WS|e|-|minimal,replica,logical||replica|p|Sets the level of information written to the WAL.
wal_log_hints|WS|b|-|||off|p|Writes full pages to WAL when first modified after a checkpoint, even for a non-critical modification.
wal_recycle|WS|b|-|||on|s|Recycles WAL files by renaming them.
wal_skip_threshold|WS|i|kB|0|2147483647|2048|u|Minimum size of new file to fsync instead of writing WAL.
wal_sync_method|WS|e|-|fdatasync,fsync,open_datasync,open_sync||fdatasync|s|Selects the method used for forcing WAL updates to disk.
wal_writer_delay|WS|i|ms|1|10000|200|s|Time between WAL flushes performed in the WAL writer.
wal_writer_flush_after|WS|i|8kB|0|2147483647|128|s|Amount of WAL written out by WAL writer that triggers a flush.
summarize_wal|WM|b|-|||off|s|Starts the WAL summarizer process to enable incremental backup.
wal_summary_keep_time|WM|i|min|0|35791394|14400|s|Time for which WAL summary files should be kept.
kingres_shard.default_bucket_count|XO|i|-|1|65536|128|u|Buckets given to a distributed table when the statement does not say.
kingres_shard.enable_distributed_scan|XO|b|-|||on|u|Lets the planner push a scan of a distributed table to the nodes that hold it.
kingres_shard.enable_routing|XO|b|-|||on|u|Routes a statement that fixes the distribution column to one node.
kingres_shard.fault_injection|XO|s|-||||u|Test hook that makes a node call fail; never set in production.
kingres_shard.forward_token|XO|s|-||||S|Per-session token of a forwarded statement.
kingres_shard.forwarded|XO|b|-|||off|S|True in a session that carries a statement forwarded by another node.
kingres_shard.move_batch_rows|XO|i|-|1|1000000|10000|u|Rows copied per batch while a bucket moves.
kingres_shard.move_lag_limit|XO|i|ms|0|3600000|1000|s|Largest replication lag at which a bucket move may switch over.
kingres_shard.node_conninfo|XO|s|-||||S|Connection options used for calls from this node to other nodes.
kingres_shard.node_timeout|XO|i|ms|0|3600000|30000|u|How long a call to another node may take before it is cancelled.
kingres_shard.scan_fetch_size|XO|i|-|1|1000000|1000|u|Rows fetched from a remote node at a time during a distributed scan.
kingres_shard.two_phase_timeout|XO|i|ms|0|3600000|60000|s|How long a prepared transaction of a distributed commit may wait for its decision.
kingres_ledger.block_interval|XO|i|ms|100|3600000|5000|s|How often the ledger closes a block of the transactions that wait.
kingres_ledger.max_block_transactions|XO|i|-|1|1000000|1000|s|The most transactions one ledger block may hold.
kingres_ledger.digest_hash|XO|e|-|sha256||sha256|i|Hash function of the ledger chain.
kingres_ledger.sign_digests|XO|b|-|||off|s|Signs every digest with the key of the cluster.
kingres_ledger.verify_batch_blocks|XO|i|-|1|100000|500|u|Blocks read per round of a verification.
`
