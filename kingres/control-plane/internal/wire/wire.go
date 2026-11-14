// SPDX-License-Identifier: PostgreSQL

// Package wire holds the request and answer types of the internal API calls
// that create, configure and remove a server's membership of a cluster
// (design control-plane-3.md, D2 to D5). It has no behavior: the leader's
// package membership and the server's package cluster both use it, and
// kingres/control-plane/internal-api.md specifies the calls.
//
// Nothing here carries a file path, a program name or SQL text; secrets are
// only password verifiers (never passwords) and certificate requests and
// certificates (never private keys).
package wire

import (
	"encoding/json"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// PeerInfo is another server as the members know it. The server id is also
// the host that PostgreSQL clients and the sharding extension use for it.
type PeerInfo struct {
	ServerID        string `json:"serverId"`
	PGPort          int    `json:"pgPort"`
	InternalAddress string `json:"internalAddress"`
}

// PrepareResponse is the answer of the bootstrap call that asks an
// uninitialized server for its certificate request.
type PrepareResponse struct {
	ServerID        string `json:"serverId"`
	PGPort          int    `json:"pgPort"`
	InternalPort    int    `json:"internalPort"`
	CSR             string `json:"csr"`
	Version         string `json:"version"`
	NodeState       string `json:"nodeState"`
	InternalVersion string `json:"internalVersion"`
}

// InstallRequest hands a server its certificate and the CA certificate.
type InstallRequest struct {
	CACert     string `json:"caCert"`
	ServerCert string `json:"serverCert"`
}

// JoinRequest is what a joining server sends to a server of the cluster
// together with an administrator's credentials. Only the certificate request
// travels, never the key.
type JoinRequest struct {
	Username        string  `json:"username"`
	Password        string  `json:"password"`
	ServerID        string  `json:"serverId"`
	CSR             string  `json:"csr"`
	PGPort          int     `json:"pgPort"`
	InternalAddress string  `json:"internalAddress"`
	ServerGroup     *string `json:"serverGroup,omitempty"`
	MemoryMegabytes int     `json:"memoryMegabytes,omitempty"`
}

// AddServerRequest is the call from the server that verified the
// administrator to the leader. Actor is that administrator's role name.
type AddServerRequest struct {
	Actor           string  `json:"actor"`
	ServerID        string  `json:"serverId"`
	CSR             string  `json:"csr"`
	PGPort          int     `json:"pgPort"`
	InternalAddress string  `json:"internalAddress"`
	ServerGroup     *string `json:"serverGroup,omitempty"`
	MemoryMegabytes int     `json:"memoryMegabytes,omitempty"`
	// Fingerprint is the SHA-256 fingerprint of the certificate the joining
	// server saw on first contact (hex); PinnedByCaller says whether the
	// administrator supplied it beforehand.
	Fingerprint    string `json:"fingerprint,omitempty"`
	PinnedByCaller bool   `json:"pinned"`
}

// JoinResponse is the answer to a join: the certificates and the task.
type JoinResponse struct {
	TaskID     int64  `json:"taskId"`
	CACert     string `json:"caCert"`
	ServerCert string `json:"serverCert"`
	ClusterID  string `json:"clusterId"`
	// ClusterName is the cluster's display name.
	ClusterName string `json:"clusterName"`
}

// CreateInstanceRequest asks a server to create and start its instance.
type CreateInstanceRequest struct {
	ClusterName     string                  `json:"clusterName"`
	MemoryMegabytes int                     `json:"memoryMegabytes"`
	Peers           []PeerInfo              `json:"peers"`
	Rules           []api.NetworkAccessRule `json:"rules"`
	TLSRequired     bool                    `json:"tlsRequired"`
}

// TrustRequest sets the other servers an instance trusts and its managed
// network rules, then reloads the instance.
type TrustRequest struct {
	Peers       []PeerInfo              `json:"peers"`
	Rules       []api.NetworkAccessRule `json:"rules"`
	TLSRequired bool                    `json:"tlsRequired"`
}

// PKIRequest installs the revocation list (and, when ServerCert is set, a
// renewed server certificate) on a server.
type PKIRequest struct {
	CACert     string   `json:"caCert"`
	ServerCert string   `json:"serverCert,omitempty"`
	Revoked    []string `json:"revoked"`
	CRL        string   `json:"crl"`
}

// PeersRequest is the whole peer directory.
type PeersRequest struct {
	Peers []PeerInfo `json:"peers"`
}

// FinishRequest completes a join on the new server.
type FinishRequest struct {
	ClusterID   string `json:"clusterId"`
	ClusterName string `json:"clusterName"`
}

// InstanceStatus is the answer of the instance status call.
type InstanceStatus struct {
	Exists  bool `json:"exists"`
	Running bool `json:"running"`
	// NodeState is the node state of the server.
	NodeState string `json:"nodeState"`
}

// ApplyRequest asks a server to run one cluster-wide operation locally.
// Kind is one of a closed set (see package cluster); Params is the kind's
// own structure.
type ApplyRequest struct {
	Kind   string          `json:"kind"`
	Params json.RawMessage `json:"params"`
}

// Empty is the answer of calls without a result.
type Empty struct{}

// RoleSpec is a role as copied between servers: its attributes and password
// verifier (never a password) and its memberships.
type RoleSpec struct {
	Name            string       `json:"name"`
	Super           bool         `json:"super"`
	Inherit         bool         `json:"inherit"`
	CreateRole      bool         `json:"createRole"`
	CreateDB        bool         `json:"createDb"`
	Login           bool         `json:"login"`
	Replication     bool         `json:"replication"`
	BypassRLS       bool         `json:"bypassRls"`
	ConnectionLimit int          `json:"connectionLimit"`
	ValidUntil      *time.Time   `json:"validUntil,omitempty"`
	Verifier        *string      `json:"verifier,omitempty"`
	MemberOf        []Membership `json:"memberOf"`
}

// Membership is membership of a role in a parent role.
type Membership struct {
	Role    string `json:"role"`
	Admin   bool   `json:"admin"`
	Inherit bool   `json:"inherit"`
	Set     bool   `json:"set"`
}

// DatabaseSpec is a database as copied between servers.
type DatabaseSpec struct {
	Name            string   `json:"name"`
	Owner           string   `json:"owner"`
	Encoding        string   `json:"encoding"`
	LcCollate       string   `json:"lcCollate"`
	LcCtype         string   `json:"lcCtype"`
	ConnectionLimit int      `json:"connectionLimit"`
	Settings        []string `json:"settings"` // name=value pairs of database-wide settings
	Extensions      []string `json:"extensions"`
	// Sharded says the database uses kingres_shard; the extension is then
	// installed by kingres.add_node, not here.
	Sharded bool `json:"sharded"`
}

// Globals is the set of global objects of a cluster.
type Globals struct {
	Roles     []RoleSpec     `json:"roles"`
	Databases []DatabaseSpec `json:"databases"`
	// Exact says Roles is the complete set and roles of the receiver that
	// are not in it (other than its own service role) are left alone anyway;
	// reserved for later use, always false now.
	Exact bool `json:"exact,omitempty"`
}
