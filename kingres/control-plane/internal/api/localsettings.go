// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package api

// ClientPorts are the client ports of a server.
type ClientPorts struct {
	// ReadWritePort is the read-write client port.
	ReadWritePort int `json:"readWritePort"`
	// ReadOnlyPort is the optional read-only port; null when disabled.
	ReadOnlyPort *int `json:"readOnlyPort"`
}

// LocalDataPaths are the directories of this server as getThisServerSettings
// answers them: the optional directories are always present, null when the
// server uses the default (inside the data directory, the system log directory).
type LocalDataPaths struct {
	DataDirectory string  `json:"dataDirectory"`
	WalDirectory  *string `json:"walDirectory"`
	LogDirectory  *string `json:"logDirectory"`
}

// LocalSettings is the body of GET and PATCH /this-server/settings.
type LocalSettings struct {
	// DataPaths are the directories of this server.
	DataPaths LocalDataPaths `json:"dataPaths"`
	// MemoryQuota is the memory budget of this server.
	MemoryQuota MemoryQuota `json:"memoryQuota"`
	// LogLevel is debug, info, warning or error.
	LogLevel string `json:"logLevel"`
	// ClientPorts are the client ports of this server.
	ClientPorts ClientPorts `json:"clientPorts"`
	// RestartRequired is true while a saved change needs a restart of the
	// instances of this server.
	RestartRequired bool `json:"restartRequired"`
}

// InstanceActionRequest is the optional body of stopThisServerInstance and
// restartThisServerInstance.
type InstanceActionRequest struct {
	// Force proceeds even when the instance holds the only up-to-date copy of a
	// shard group.
	Force bool `json:"force"`
}
