// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NodeMap is the name of the pg_ident.conf map that lets the servers of the
// cluster authenticate with their certificates (design control-plane-3.md,
// D3): the certificate name of any server of the cluster is mapped to any
// database role.
const NodeMap = "kingres_nodes"

// TrustFile is the name of the file in an instance directory that records the
// other servers the instance trusts. It is public information.
const TrustFile = "kingres-nodes.json"

// SettingsFile is the name of the file that records the managed settings the
// instance was last configured with (kingres-managed.conf is rendered from it).
const SettingsFile = "kingres-settings.json"

// NodePeer is another server of the cluster: its id (the common name of its
// certificate) and the networks its connections arrive from.
type NodePeer struct {
	ServerID string   `json:"serverId"`
	CIDRs    []string `json:"cidrs"`
}

// NodeTrust is the set of servers an instance accepts certificate
// authenticated connections from.
type NodeTrust struct {
	Peers []NodePeer `json:"peers"`
}

// Validate checks every peer.
func (t NodeTrust) Validate() error {
	if len(t.Peers) > 1000 {
		return invalid("too many peers")
	}
	for _, p := range t.Peers {
		if _, err := hbaName(p.ServerID); err != nil || p.ServerID == "all" {
			return invalid("server id %q cannot be written into pg_ident.conf", trunc(p.ServerID))
		}
		if len(p.CIDRs) == 0 {
			return invalid("server %q has no address", trunc(p.ServerID))
		}
		for _, c := range p.CIDRs {
			if _, err := netip.ParsePrefix(c); err != nil {
				return invalid("address %q is not a CIDR network", trunc(c))
			}
		}
	}
	return nil
}

// hbaLines renders the node rules. They come first in pg_hba.conf: a client
// that presents no certificate cannot match a "cert" rule and fails there
// instead of falling through, so the rules only ever apply to TLS connections
// from the servers' networks.
func (t NodeTrust) hbaLines() string {
	seen := map[string]bool{}
	var cidrs []string
	for _, p := range t.Peers {
		for _, c := range p.CIDRs {
			pf, err := netip.ParsePrefix(c)
			if err != nil {
				continue
			}
			s := pf.Masked().String()
			if !seen[s] {
				seen[s] = true
				cidrs = append(cidrs, s)
			}
		}
	}
	sort.Strings(cidrs)
	var sb strings.Builder
	for _, c := range cidrs {
		fmt.Fprintf(&sb, "hostssl   all all %s cert map=%s\n", c, NodeMap)
	}
	// Replication connections (pg_basebackup and the walreceiver of a standby, design
	// ha-phase5 part A) are matched by their own database keyword: "all" never covers
	// "replication". The same certificate map authenticates them.
	for _, c := range cidrs {
		fmt.Fprintf(&sb, "hostssl   replication all %s cert map=%s\n", c, NodeMap)
	}
	return sb.String()
}

// identLines renders the map: every server's certificate name to any role.
func (t NodeTrust) identLines() string {
	ids := make([]string, 0, len(t.Peers))
	for _, p := range t.Peers {
		ids = append(ids, p.ServerID)
	}
	sort.Strings(ids)
	var sb strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&sb, "%s \"%s\" all\n", NodeMap, id)
	}
	return sb.String()
}

// Trust returns the recorded trust of instance id (empty when none).
func (m *Manager) Trust(id int) (NodeTrust, error) {
	dir, err := m.Dir(id)
	if err != nil {
		return NodeTrust{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, TrustFile)) //nolint:gosec // fixed name below the manager root
	if errors.Is(err, os.ErrNotExist) {
		return NodeTrust{}, nil
	}
	if err != nil {
		return NodeTrust{}, err
	}
	var t NodeTrust
	if err := json.Unmarshal(b, &t); err != nil {
		return NodeTrust{}, fmt.Errorf("instance: %s: %w", TrustFile, err)
	}
	return t, t.Validate()
}

// SetTrust records the trust of instance id; pg_hba.conf and pg_ident.conf are
// rewritten by WriteHBA and WriteIdent, which the caller then calls and reloads.
func (m *Manager) SetTrust(id int, t NodeTrust) error {
	if _, err := m.Managed(id); err != nil {
		return err
	}
	if err := t.Validate(); err != nil {
		return err
	}
	dir, _ := m.Dir(id)
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, TrustFile), b)
}

// Settings returns the managed settings recorded for instance id.
func (m *Manager) Settings(id int) (ManagedSettings, error) {
	dir, err := m.Dir(id)
	if err != nil {
		return ManagedSettings{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, SettingsFile)) //nolint:gosec // fixed name below the manager root
	if err != nil {
		return ManagedSettings{}, err
	}
	var s ManagedSettings
	if err := json.Unmarshal(b, &s); err != nil {
		return ManagedSettings{}, fmt.Errorf("instance: %s: %w", SettingsFile, err)
	}
	return s, nil
}

func (m *Manager) saveSettings(id int, s ManagedSettings) error {
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, SettingsFile), b)
}

// Remove stops the instance and deletes its directory and socket directory.
// Only a directory with our marker is touched; removing an instance that does
// not exist is not an error.
func (m *Manager) Remove(ctx context.Context, id int) error {
	if _, err := m.Managed(id); err != nil {
		if errors.Is(err, ErrNotManaged) {
			if dir, derr := m.Dir(id); derr == nil {
				if _, serr := os.Lstat(dir); errors.Is(serr, os.ErrNotExist) {
					return nil
				}
			}
		}
		return err
	}
	if err := m.Stop(ctx, id); err != nil {
		return err
	}
	dir, _ := m.Dir(id)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("instance: removing %s: %w", dir, err)
	}
	if sock, err := m.SocketDir(id); err == nil && m.sockBase != "" {
		_ = os.RemoveAll(sock)
	}
	return nil
}
