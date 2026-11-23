// SPDX-License-Identifier: AGPL-3.0-only

// Package problem implements the error model of the REST API: RFC 9457
// "problem details" documents served as application/problem+json, exactly as
// the Problem schema of the contract defines them.
//
// Every error response of the server is produced by Write; handlers return a
// *Problem (which implements error) and never format error bodies themselves.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// MediaType is the content type of every error response.
const MediaType = "application/problem+json"

// typeBase is the prefix of the "type" URIs used by the contract examples.
const typeBase = "https://kingresdb.dev/problems/"

// Code is a value of the ProblemCode enum of the contract.
type Code string

// The problem codes of the contract (schema ProblemCode).
const (
	BadRequest             Code = "bad_request"
	ValidationFailed       Code = "validation_failed"
	Unauthenticated        Code = "unauthenticated"
	InvalidCredentials     Code = "invalid_credentials"
	SessionExpired         Code = "session_expired"
	CSRFTokenInvalid       Code = "csrf_token_invalid"
	Forbidden              Code = "forbidden"
	SetupTokenInvalid      Code = "setup_token_invalid"
	NotFound               Code = "not_found"
	MethodNotAllowed       Code = "method_not_allowed"
	Conflict               Code = "conflict"
	AlreadyExists          Code = "already_exists"
	ImmutableField         Code = "immutable_field"
	RebalanceInProgress    Code = "rebalance_in_progress"
	OperationInProgress    Code = "operation_in_progress"
	TaskNotCancellable     Code = "task_not_cancellable"
	LastAdmin              Code = "last_admin"
	ServerOwnsBuckets      Code = "server_owns_buckets"
	ServerHostsMapGroup    Code = "server_hosts_map_group"
	LastServer             Code = "last_server"
	RoleInUse              Code = "role_in_use"
	OptimisticLockFail     Code = "optimistic_lock_failed"
	NoReplicaAvailable     Code = "no_replica_available"
	InstanceAlreadyRunning Code = "instance_already_running"
	InstanceAlreadyStopped Code = "instance_already_stopped"
	SystemDatabaseStopped  Code = "system_database_stopped"
	LedgerEntryPending     Code = "ledger_entry_pending"
	QuorumUnavailable      Code = "quorum_unavailable"
	ServerGroupNotEmpty    Code = "server_group_not_empty"
	Unprocessable          Code = "unprocessable"
	SQLError               Code = "sql_error"
	UnsupportedOp          Code = "unsupported_operation"
	NodeStateInvalid       Code = "node_state_invalid"
	NodeUninitialized      Code = "node_uninitialized"
	MapGroupUnavailable    Code = "map_group_unavailable"
	LeaderUnavailable      Code = "leader_unavailable"
	RateLimited            Code = "rate_limited"
	ServiceUnavailable     Code = "service_unavailable"
	// WorkbenchBusy (429) was added in contract 0.7.0: too many statements run on this server.
	WorkbenchBusy Code = "workbench_busy"
	InternalError Code = "internal_error"
	// CertificateAuthorityUnavailable (503) and CertificateRevoked (409) were added in contract 0.5.0.
	CertificateAuthorityUnavailable Code = "certificate_authority_unavailable"
	CertificateRevoked              Code = "certificate_revoked"
	// InternalAPIDisabled (409) was added in contract 0.6.0: the server runs without its internal API.
	InternalAPIDisabled Code = "internal_api_disabled"
	// Added in contract 0.8.0 (configuration from the console).
	ConfigParameterUnknown Code = "config_parameter_unknown"
	ConfigParameterManaged Code = "config_parameter_managed"
	ConfigParameterRefused Code = "config_parameter_refused"
	ConfigLevelNotAllowed  Code = "config_level_not_allowed"
	ConfigValueBelowFloor  Code = "config_value_below_floor"
	ConfigChangeSuperseded Code = "config_change_superseded"
	ConfigRestartOrder     Code = "config_restart_order_conflict"
	NothingPending         Code = "nothing_pending"
	// Added in contract 0.9.0 (rebalancing): the reasons a rebalance does not start.
	NothingToRebalance  Code = "nothing_to_rebalance"
	RebalanceNotRunning Code = "rebalance_not_running"
	ClusterInconsistent Code = "cluster_inconsistent"
	VersionMismatch     Code = "version_mismatch"
	FailoverInProgress  Code = "failover_in_progress"
	GroupDegraded       Code = "group_degraded"
	InsufficientDisk    Code = "insufficient_disk"
	NoReceivingServer   Code = "no_receiving_server"
	// ReplicasCannotBeKept: a drain would leave fewer servers than the replica count needs (contract 0.15.0).
	ReplicasCannotBeKept Code = "replicas_cannot_be_kept"
	// Distributed schemas (contract 0.15.0).
	ShardingModeInUse      Code = "sharding_mode_in_use"
	SchemaNotDistributable Code = "schema_not_distributable"
	SchemaPinned           Code = "schema_pinned"
	SchemaMoveInProgress   Code = "schema_move_in_progress"
	// BucketBusy: the bucket has a move or a split in progress (contract 0.15.0).
	BucketBusy Code = "bucket_busy"
	// SplitUnsupported: a table of the colocation group cannot be split (contract 0.15.0).
	SplitUnsupported Code = "split_unsupported"
	// Added in contract 0.10.0 (failover).
	FailoverNeedsDecision Code = "failover_needs_decision"
	LeaseSettingsDiffer   Code = "lease_settings_differ"
	// Added in contract 0.11.0 (ledger phase 7).
	LedgerNotPublished Code = "ledger_not_published"
	OriginPrefixLocked Code = "origin_prefix_locked"
	SigningKeyMissing  Code = "signing_key_missing"
	LastSigningKey     Code = "last_signing_key"
	ChainNotDiverged   Code = "chain_not_diverged"
	// Added in contract 0.14.0 (the table lifecycle): the table cannot take the requested change; Blockers says why.
	TableNotEligible Code = "table_not_eligible"
	// Added in contract 0.17.0 (phase 12): the extensions of the analytics, the storage format and the time-partitioning policy.
	ExtensionUnavailable     Code = "extension_unavailable"
	ExtensionVersionMismatch Code = "extension_version_mismatch"
	SketchHashMismatch       Code = "sketch_hash_mismatch"
	ExtensionMissing         Code = "extension_missing"
	ConversionRefused        Code = "conversion_refused"
	PartitionPolicyConflict  Code = "partition_policy_conflict"
	// Added in contract 0.19.0 (data transfer).
	DataTransferDisabled   Code = "data_transfer_disabled"
	InsecureTransport      Code = "insecure_transport"
	TransferQuotaExceeded  Code = "transfer_quota_exceeded"
	UploadIncomplete       Code = "upload_incomplete"
	UploadOffsetMismatch   Code = "upload_offset_mismatch"
	ChunkTooLarge          Code = "chunk_too_large"
	UploadChecksumMismatch Code = "upload_checksum_mismatch"
	ImportHeaderMismatch   Code = "import_header_mismatch"
	ImportLedgerPolicy     Code = "import_ledger_policy"
	ImportTooLarge         Code = "import_too_large"
	ImportEndMarker        Code = "import_end_marker"
	ExportUntrustedCode    Code = "export_untrusted_code"

	// Contract 0.40.0 (database export and archive validation).
	ExportNotReadable   Code = "export_not_readable"
	LargeObjectsPresent Code = "large_objects_present"
	SchemaUnrecognised  Code = "schema_unrecognised"
	ArchiveInvalid      Code = "archive_invalid"
	ArchiveIncompatible Code = "archive_incompatible"

	// Contract 0.41.0 (database import).
	ArchiveStatementRefused      Code = "archive_statement_refused"
	LedgerRestartNotAcknowledged Code = "ledger_restart_not_acknowledged"
)

// FieldError is one entry of the "errors" extension (schema ValidationIssue).
type FieldError struct {
	// Field is a JSON Pointer to the invalid body property, or the name of an
	// invalid query or path parameter.
	Field string `json:"field"`
	// Message explains the problem.
	Message string `json:"message"`
	// Code is a short machine-readable reason such as "required".
	Code string `json:"code,omitempty"`
}

// Problem is an RFC 9457 problem details document (schema Problem). It is
// also an error, so handlers can return it.
type Problem struct {
	// Type identifies the problem type (URI reference).
	Type string `json:"type"`
	// Title is a short, constant summary of the problem type.
	Title string `json:"title"`
	// Status is the HTTP status code of the response.
	Status int `json:"status"`
	// Detail explains this occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance is the path of the request that caused the problem.
	Instance string `json:"instance,omitempty"`
	// Code is the machine-readable code clients branch on.
	Code Code `json:"code"`
	// RequestID is the value of the X-Request-Id response header.
	RequestID string `json:"requestId,omitempty"`
	// Errors are field-level problems for validation_failed.
	Errors []FieldError `json:"errors,omitempty"`
	// SQLState is the PostgreSQL SQLSTATE for sql_error.
	SQLState string `json:"sqlState,omitempty"`
	// SQLPosition is the 1-based error position for sql_error.
	SQLPosition int `json:"sqlPosition,omitempty"`
	// ServerIDs are servers that can be opened instead (leader_unavailable).
	ServerIDs []string `json:"serverIds,omitempty"`
	// Plan is the plan of a failover request that needs a decision (failover_needs_decision; contract 0.12.0).
	Plan any `json:"plan,omitempty"`
	// Blockers are the lines of the report of the sharding extension that make a table not eligible (table_not_eligible; contract 0.14.0).
	Blockers any `json:"blockers,omitempty"`
	// Reasons are further reasons in the shape of Blockers (contract 0.19.0, optional).
	Reasons any `json:"reasons,omitempty"`

	// retryAfter, when positive, is sent as a Retry-After header (seconds).
	retryAfter int
	// headers are extra response headers such as WWW-Authenticate or Allow.
	headers map[string]string
}

// Error implements error.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return string(p.Code) + ": " + p.Detail
	}
	return string(p.Code) + ": " + p.Title
}

// specs maps each code to its HTTP status, title and type slug. The statuses
// follow the table in kingres/api/README.md.
var specs = map[Code]struct {
	status int
	title  string
	slug   string
}{
	BadRequest:                      {400, "Bad request", "bad-request"},
	ValidationFailed:                {400, "Validation failed", "validation-failed"},
	Unauthenticated:                 {401, "Authentication required", "unauthenticated"},
	InvalidCredentials:              {401, "Invalid credentials", "invalid-credentials"},
	SessionExpired:                  {401, "Session expired", "session-expired"},
	SetupTokenInvalid:               {401, "Setup token invalid", "setup-token-invalid"},
	CSRFTokenInvalid:                {403, "CSRF token invalid", "csrf-token-invalid"},
	Forbidden:                       {403, "Forbidden", "forbidden"},
	NotFound:                        {404, "Not found", "not-found"},
	MethodNotAllowed:                {405, "Method not allowed", "method-not-allowed"},
	Conflict:                        {409, "Conflict", "conflict"},
	AlreadyExists:                   {409, "Already exists", "already-exists"},
	ImmutableField:                  {409, "Immutable field", "immutable-field"},
	RebalanceInProgress:             {409, "Rebalance in progress", "rebalance-in-progress"},
	OperationInProgress:             {409, "Operation in progress", "operation-in-progress"},
	TaskNotCancellable:              {409, "Task not cancellable", "task-not-cancellable"},
	LastAdmin:                       {409, "Last administrator", "last-admin"},
	ServerOwnsBuckets:               {409, "Server owns buckets", "server-owns-buckets"},
	ServerHostsMapGroup:             {409, "Server hosts the map group", "server-hosts-map-group"},
	LastServer:                      {409, "Last server", "last-server"},
	RoleInUse:                       {409, "Role in use", "role-in-use"},
	OptimisticLockFail:              {409, "Optimistic lock failed", "optimistic-lock-failed"},
	NoReplicaAvailable:              {409, "No replica available", "no-replica-available"},
	InstanceAlreadyRunning:          {409, "Instance already running", "instance-already-running"},
	InstanceAlreadyStopped:          {409, "Instance already stopped", "instance-already-stopped"},
	SystemDatabaseStopped:           {503, "System database stopped", "system-database-stopped"},
	LedgerEntryPending:              {409, "Ledger entry pending", "ledger-entry-pending"},
	ServerGroupNotEmpty:             {409, "Server group not empty", "server-group-not-empty"},
	Unprocessable:                   {422, "Unprocessable request", "unprocessable"},
	SQLError:                        {422, "SQL error", "sql-error"},
	UnsupportedOp:                   {422, "Operation not supported yet", "unsupported-operation"},
	NodeStateInvalid:                {422, "Invalid node state", "node-state-invalid"},
	RateLimited:                     {429, "Too many requests", "rate-limited"},
	NodeUninitialized:               {503, "Node uninitialized", "node-uninitialized"},
	MapGroupUnavailable:             {503, "Map group unavailable", "map-group-unavailable"},
	LeaderUnavailable:               {503, "Leader unavailable", "leader-unavailable"},
	QuorumUnavailable:               {503, "Quorum unavailable", "quorum-unavailable"},
	ServiceUnavailable:              {503, "Service unavailable", "service-unavailable"},
	WorkbenchBusy:                   {429, "Too many statements", "workbench-busy"},
	InternalError:                   {500, "Internal error", "internal-error"},
	CertificateAuthorityUnavailable: {503, "Certificate authority unavailable", "certificate-authority-unavailable"},
	CertificateRevoked:              {409, "Certificate revoked", "certificate-revoked"},
	InternalAPIDisabled:             {409, "Internal API disabled", "internal-api-disabled"},
	ConfigParameterUnknown:          {404, "Unknown parameter", "config-parameter-unknown"},
	ConfigParameterManaged:          {422, "Managed parameter", "config-parameter-managed"},
	ConfigParameterRefused:          {422, "Refused parameter", "config-parameter-refused"},
	ConfigLevelNotAllowed:           {422, "Level not allowed", "config-level-not-allowed"},
	ConfigValueBelowFloor:           {422, "Value below the floor", "config-value-below-floor"},
	ConfigChangeSuperseded:          {409, "Change superseded", "config-change-superseded"},
	ConfigRestartOrder:              {409, "No safe restart order", "config-restart-order-conflict"},
	NothingPending:                  {409, "Nothing pending", "nothing-pending"},
	NothingToRebalance:              {409, "Nothing to rebalance", "nothing-to-rebalance"},
	RebalanceNotRunning:             {409, "No rebalance running", "rebalance-not-running"},
	ClusterInconsistent:             {409, "Cluster inconsistent", "cluster-inconsistent"},
	VersionMismatch:                 {409, "Version mismatch", "version-mismatch"},
	FailoverInProgress:              {409, "Failover in progress", "failover-in-progress"},
	GroupDegraded:                   {409, "Shard group degraded", "group-degraded"},
	InsufficientDisk:                {409, "Insufficient disk space", "insufficient-disk"},
	NoReceivingServer:               {409, "No receiving server", "no-receiving-server"},
	ReplicasCannotBeKept:            {409, "Replicas cannot be kept", "replicas-cannot-be-kept"},
	ShardingModeInUse:               {409, "Sharding mode in use", "sharding-mode-in-use"},
	SchemaNotDistributable:          {422, "Schema not distributable", "schema-not-distributable"},
	SchemaPinned:                    {409, "Schema pinned", "schema-pinned"},
	SchemaMoveInProgress:            {409, "Schema move in progress", "schema-move-in-progress"},
	BucketBusy:                      {409, "Bucket busy", "bucket-busy"},
	SplitUnsupported:                {422, "Split not supported", "split-unsupported"},
	FailoverNeedsDecision:           {409, "The failover needs a decision", "failover-needs-decision"},
	LeaseSettingsDiffer:             {409, "Lease settings differ", "lease-settings-differ"},
	LedgerNotPublished:              {409, "The ledger is not published", "ledger-not-published"},
	OriginPrefixLocked:              {409, "The origin prefix is locked", "origin-prefix-locked"},
	SigningKeyMissing:               {409, "No signing key", "signing-key-missing"},
	LastSigningKey:                  {409, "Last signing key", "last-signing-key"},
	ChainNotDiverged:                {409, "The chain is not diverged", "chain-not-diverged"},
	TableNotEligible:                {422, "The table cannot take this change", "table-not-eligible"},
	ExtensionUnavailable:            {422, "The extension is not available", "extension-unavailable"},
	ExtensionVersionMismatch:        {409, "The extension versions differ", "extension-version-mismatch"},
	SketchHashMismatch:              {409, "The sketch hashes differ", "sketch-hash-mismatch"},
	ExtensionMissing:                {409, "An extension is missing", "extension-missing"},
	ConversionRefused:               {409, "The storage format cannot be changed", "conversion-refused"},
	PartitionPolicyConflict:         {409, "The table cannot have this policy", "partition-policy-conflict"},
	DataTransferDisabled:            {403, "Data transfer is disabled", "data-transfer-disabled"},
	InsecureTransport:               {403, "Insecure transport", "insecure-transport"},
	TransferQuotaExceeded:           {409, "Staging quota exceeded", "transfer-quota-exceeded"},
	UploadIncomplete:                {409, "Upload incomplete", "upload-incomplete"},
	UploadOffsetMismatch:            {409, "Upload offset mismatch", "upload-offset-mismatch"},
	ChunkTooLarge:                   {413, "Chunk too large", "chunk-too-large"},
	UploadChecksumMismatch:          {422, "Upload checksum mismatch", "upload-checksum-mismatch"},
	ImportHeaderMismatch:            {422, "The header does not match", "import-header-mismatch"},
	ImportLedgerPolicy:              {422, "The ledger policy refuses unsigned writes", "import-ledger-policy"},
	ImportTooLarge:                  {422, "The file is too large to import", "import-too-large"},
	ImportEndMarker:                 {422, "The file has an end-of-data line", "import-end-marker"},
	ExportUntrustedCode:             {422, "The read would run code of another role", "export-untrusted-code"},
	ExportNotReadable:               {422, "The role cannot read every table", "export-not-readable"},
	LargeObjectsPresent:             {422, "The database has large objects", "large-objects-present"},
	SchemaUnrecognised:              {422, "The schema has an object the archive does not carry", "schema-unrecognised"},
	ArchiveInvalid:                  {422, "The archive is not valid", "archive-invalid"},
	ArchiveIncompatible:             {422, "The archive does not fit this cluster", "archive-incompatible"},
	ArchiveStatementRefused:         {422, "The archive has a statement that is not allowed", "archive-statement-refused"},
	LedgerRestartNotAcknowledged:    {422, "The import restarts the ledger chains", "ledger-restart-not-acknowledged"},
}

// New returns a Problem for the code with the standard status, title and
// type, and the given occurrence-specific detail. An unknown code becomes
// internal_error.
func New(code Code, detail string) *Problem {
	s, ok := specs[code]
	if !ok {
		s = specs[InternalError]
		code = InternalError
	}
	return &Problem{
		Type:   typeBase + s.slug,
		Title:  s.title,
		Status: s.status,
		Detail: detail,
		Code:   code,
	}
}

// Validation returns a validation_failed problem carrying field errors.
func Validation(detail string, errs ...FieldError) *Problem {
	p := New(ValidationFailed, detail)
	p.Errors = errs
	return p
}

// SQL returns a sql_error problem with the SQLSTATE and, if positive, the
// 1-based position of the error in the statement.
func SQL(detail, sqlState string, position int) *Problem {
	p := New(SQLError, detail)
	p.SQLState = sqlState
	p.SQLPosition = position
	return p
}

// WithRetryAfter sets the Retry-After header in seconds and returns p.
func (p *Problem) WithRetryAfter(seconds int) *Problem {
	p.retryAfter = seconds
	return p
}

// WithHeader adds a response header and returns p.
func (p *Problem) WithHeader(name, value string) *Problem {
	if p.headers == nil {
		p.headers = map[string]string{}
	}
	p.headers[name] = value
	return p
}

// Write sends p as application/problem+json. instance is the request path
// and requestID the value of the X-Request-Id header; both are copied into
// the body. It is the single place where error bodies are written.
func Write(w http.ResponseWriter, instance, requestID string, p *Problem) {
	out := *p
	out.Instance = instance
	out.RequestID = requestID
	body, err := json.Marshal(&out)
	if err != nil {
		// A Problem holds only strings and integers; this cannot happen.
		body = []byte(`{"type":"about:blank","title":"Internal error","status":500,"code":"internal_error"}`)
		out.Status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", MediaType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	for k, v := range p.headers {
		h.Set(k, v)
	}
	if p.retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.retryAfter))
	}
	w.WriteHeader(out.Status)
	_, _ = w.Write(append(body, '\n'))
}

// WithBlockers attaches the blockers of a table_not_eligible problem (contract 0.14.0).
func (p *Problem) WithBlockers(b any) *Problem {
	p.Blockers = b
	return p
}

// WithReasons attaches the reasons of an archive problem or of export_not_readable, in the shape of the blockers (contract 0.40.0).
func (p *Problem) WithReasons(r any) *Problem {
	p.Reasons = r
	return p
}

// WithPlan attaches the plan of a failover request to a failover_needs_decision problem (contract 0.12.0).
func (p *Problem) WithPlan(plan any) *Problem {
	p.Plan = plan
	return p
}
