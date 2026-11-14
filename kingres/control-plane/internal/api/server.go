// SPDX-License-Identifier: PostgreSQL

package api

// ServiceStatus is the state of a local service.
type ServiceStatus struct {
	Name   string  `json:"name"`
	State  string  `json:"state"`
	Since  *Time   `json:"since,omitempty"`
	Detail *string `json:"detail,omitempty"`
}

// LocalServerInfo is the response of GET /this-server.
type LocalServerInfo struct {
	ServerID        *string         `json:"serverId"`
	Address         string          `json:"address"`
	Hostname        string          `json:"hostname"`
	Version         string          `json:"version"`
	PostgresVersion *string         `json:"postgresVersion"`
	NodeState       string          `json:"nodeState"`
	ClusterID       *string         `json:"clusterId"`
	UptimeSeconds   int64           `json:"uptimeSeconds"`
	IsManagerLeader bool            `json:"isManagerLeader"`
	Services        []ServiceStatus `json:"services"`
}

// CPUUsage is CPU usage; null means not available.
type CPUUsage struct {
	Cores         *int     `json:"cores"`
	UsagePercent  *float64 `json:"usagePercent"`
	LoadAverage1m *float64 `json:"loadAverage1m"`
}

// MemoryUsage is memory usage; null means not available.
type MemoryUsage struct {
	TotalBytes *int64 `json:"totalBytes"`
	UsedBytes  *int64 `json:"usedBytes"`
	QuotaBytes *int64 `json:"quotaBytes"`
}

// DiskUsage is the usage of one directory's file system.
type DiskUsage struct {
	Path       string `json:"path"`
	Purpose    string `json:"purpose"`
	TotalBytes *int64 `json:"totalBytes"`
	UsedBytes  *int64 `json:"usedBytes"`
}

// ServerResources is the response of GET /this-server/resources.
type ServerResources struct {
	CPU       CPUUsage    `json:"cpu"`
	Memory    MemoryUsage `json:"memory"`
	Disks     []DiskUsage `json:"disks"`
	SampledAt Time        `json:"sampledAt"`
}

// LogEntry is one log line.
type LogEntry struct {
	Timestamp  Time    `json:"timestamp"`
	Level      string  `json:"level"`
	Component  string  `json:"component"`
	ServerID   *string `json:"serverId"`
	InstanceID *int32  `json:"instanceId"`
	Message    string  `json:"message"`
}

// LogPage is a page of log entries.
type LogPage struct {
	Items      []LogEntry `json:"items"`
	NextCursor *string    `json:"nextCursor"`
}

// Alert is an alert of the health checks.
type Alert struct {
	ID             int64   `json:"id"`
	Kind           string  `json:"kind"`
	Severity       string  `json:"severity"`
	State          string  `json:"state"`
	Title          string  `json:"title"`
	Message        string  `json:"message"`
	ServerID       *string `json:"serverId"`
	DatabaseName   *string `json:"databaseName"`
	RaisedAt       Time    `json:"raisedAt"`
	AcknowledgedAt *Time   `json:"acknowledgedAt"`
	AcknowledgedBy *string `json:"acknowledgedBy"`
	ResolvedAt     *Time   `json:"resolvedAt"`
}

// AlertPage is a page of alerts.
type AlertPage struct {
	Items      []Alert `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// MetricPoint is one value; null means not available.
type MetricPoint struct {
	Timestamp Time     `json:"timestamp"`
	Value     *float64 `json:"value"`
}

// MetricSeries is one series with its labels.
type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Points []MetricPoint     `json:"points"`
}

// MetricQueryResult is the response of GET /metrics/query.
type MetricQueryResult struct {
	Metric string         `json:"metric"`
	Unit   string         `json:"unit"`
	From   Time           `json:"from"`
	To     Time           `json:"to"`
	Step   int            `json:"step"`
	Series []MetricSeries `json:"series"`
}
