// SPDX-License-Identifier: AGPL-3.0-only

package api

// The types of the Configuration operations (contract 0.8.0, design config-phase13.md).

// ConfigOverride is a value set at one level.
type ConfigOverride struct {
	Level     string  `json:"level"`
	ScopeID   *string `json:"scopeId"`
	Value     string  `json:"value"`
	Revision  int64   `json:"revision"`
	UpdatedBy string  `json:"updatedBy"`
	UpdatedAt Time    `json:"updatedAt"`
}

// ConfigServerValue is one parameter on one server.
type ConfigServerValue struct {
	ServerID     string  `json:"serverId"`
	Value        *string `json:"value"`
	Source       string  `json:"source"`
	Level        *string `json:"level"`
	State        string  `json:"state"`
	PendingValue *string `json:"pendingValue"`
	Error        *string `json:"error"`
}

// ConfigParameter is a PostgreSQL parameter with its metadata and values.
type ConfigParameter struct {
	Name            string              `json:"name"`
	Category        string              `json:"category"`
	Description     string              `json:"description"`
	Type            string              `json:"type"`
	Unit            *string             `json:"unit"`
	Min             *string             `json:"min"`
	Max             *string             `json:"max"`
	EnumValues      []string            `json:"enumValues"`
	Context         string              `json:"context"`
	RestartRequired bool                `json:"restartRequired"`
	DefaultValue    *string             `json:"defaultValue"`
	Kind            string              `json:"kind"`
	Reason          *string             `json:"reason"`
	Hidden          bool                `json:"hidden"`
	AllowedLevels   []string            `json:"allowedLevels"`
	ClusterOnly     *string             `json:"clusterOnly"`
	Overrides       []ConfigOverride    `json:"overrides"`
	Servers         []ConfigServerValue `json:"servers"`
	Differs         bool                `json:"differs"`
	DiffersInGroup  bool                `json:"differsInGroup"`
}

// ConfigParameterList is a page of parameters.
type ConfigParameterList struct {
	Items          []ConfigParameter `json:"items"`
	NextCursor     *string           `json:"nextCursor"`
	Revision       int64             `json:"revision"`
	Partial        bool              `json:"partial,omitempty"`
	MissingServers []MissingServer   `json:"missingServers,omitempty"`
}

// ConfigChangeItem is one value to set or remove.
type ConfigChangeItem struct {
	Name    string  `json:"name"`
	Level   string  `json:"level"`
	ScopeID *string `json:"scopeId,omitempty"`
	Value   *string `json:"value"`
}

// ConfigChangeRequest is the body of previewConfigChange and applyConfigChange.
type ConfigChangeRequest struct {
	Changes []ConfigChangeItem `json:"changes"`
}

// ConfigProblem says why one change is not valid.
type ConfigProblem struct {
	Index   int    `json:"index"`
	Pointer string `json:"pointer"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ConfigEffect is what a change does to one parameter on one server.
type ConfigEffect struct {
	ServerID    string  `json:"serverId"`
	Name        string  `json:"name"`
	Before      *string `json:"before"`
	After       *string `json:"after"`
	TakesEffect string  `json:"takesEffect"`
}

// ConfigRestartStep is one instance of a restart order.
type ConfigRestartStep struct {
	ServerID   string `json:"serverId"`
	InstanceID int32  `json:"instanceId"`
	Role       string `json:"role"`
}

// ConfigChangePreview is the answer of previewConfigChange.
type ConfigChangePreview struct {
	Valid                bool                `json:"valid"`
	Problems             []ConfigProblem     `json:"problems"`
	Effects              []ConfigEffect      `json:"effects"`
	RestartServerIDs     []string            `json:"restartServerIds"`
	RestartOrder         []ConfigRestartStep `json:"restartOrder"`
	RestartOrderConflict *string             `json:"restartOrderConflict"`
}

// ConfigChange is one recorded change.
type ConfigChange struct {
	ChangeID   int64   `json:"changeId"`
	Revision   int64   `json:"revision"`
	Name       string  `json:"name"`
	Level      string  `json:"level"`
	ScopeID    *string `json:"scopeId"`
	OldValue   *string `json:"oldValue"`
	NewValue   *string `json:"newValue"`
	ChangedBy  string  `json:"changedBy"`
	ChangedAt  Time    `json:"changedAt"`
	TaskID     *int64  `json:"taskId"`
	Reverts    *int64  `json:"reverts"`
	Revertible bool    `json:"revertible"`
	TaskState  *string `json:"taskState"`
}

// ConfigChangeList is a page of changes.
type ConfigChangeList struct {
	Items      []ConfigChange `json:"items"`
	NextCursor *string        `json:"nextCursor"`
}

// ConfigServerFailure is a parameter that failed on a server.
type ConfigServerFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// ConfigServerStatus is the configuration state of one server.
type ConfigServerStatus struct {
	ServerID        string                `json:"serverId"`
	ServerGroup     *string               `json:"serverGroup"`
	AppliedRevision int64                 `json:"appliedRevision"`
	State           string                `json:"state"`
	PendingRestart  []string              `json:"pendingRestart"`
	Failed          []ConfigServerFailure `json:"failed"`
	OverrideCount   int                   `json:"overrideCount"`
	Detail          *string               `json:"detail"`
}

// ConfigServerList is the answer of listConfigServers.
type ConfigServerList struct {
	Items    []ConfigServerStatus `json:"items"`
	Revision int64                `json:"revision"`
}

// ClockDifference is the largest clock difference of the summary.
type ClockDifference struct {
	ServerID     *string  `json:"serverId"`
	DifferenceMs *float64 `json:"differenceMs"`
}

// ConfigSummary is the answer of getConfigSummary.
type ConfigSummary struct {
	Revision               int64           `json:"revision"`
	ServersPendingRestart  []string        `json:"serversPendingRestart"`
	DifferingInGroup       []string        `json:"differingInGroup"`
	FailedServerIDs        []string        `json:"failedServerIds"`
	WaitingServerIDs       []string        `json:"waitingServerIds"`
	LargestClockDifference ClockDifference `json:"largestClockDifference"`
	MaxDifferenceMs        int             `json:"maxDifferenceMs"`
}

// ServerTime is the clock and the zones of one server.
type ServerTime struct {
	ServerID      string   `json:"serverId"`
	Reachable     bool     `json:"reachable"`
	Clock         *Time    `json:"clock"`
	DifferenceMs  *float64 `json:"differenceMs"`
	UncertaintyMs *float64 `json:"uncertaintyMs"`
	RoundTripMs   *float64 `json:"roundTripMs"`
	OSTimeZone    *string  `json:"osTimeZone"`
	TimeZone      *string  `json:"timeZone"`
	LogTimeZone   *string  `json:"logTimeZone"`
}

// DatabaseTimeZone is the time zone set for a database.
type DatabaseTimeZone struct {
	Name     string  `json:"name"`
	TimeZone *string `json:"timeZone"`
}

// ClusterTime is the answer of getClusterTime.
type ClusterTime struct {
	MeasuredAt        Time               `json:"measuredAt"`
	ReferenceServerID string             `json:"referenceServerId"`
	MaxDifferenceMs   int                `json:"maxDifferenceMs"`
	Servers           []ServerTime       `json:"servers"`
	Databases         []DatabaseTimeZone `json:"databases"`
}

// TimeZoneInfo is one time zone name.
type TimeZoneInfo struct {
	Name             string `json:"name"`
	UTCOffset        string `json:"utcOffset"`
	IsDaylightSaving bool   `json:"isDaylightSaving"`
}

// TimeZoneList is the answer of listTimeZones.
type TimeZoneList struct {
	Items []TimeZoneInfo `json:"items"`
}

// RestartPendingRequest is the body of restartPendingInstances.
type RestartPendingRequest struct {
	AcknowledgeDowntime bool     `json:"acknowledgeDowntime"`
	ServerIDs           []string `json:"serverIds,omitempty"`
}
