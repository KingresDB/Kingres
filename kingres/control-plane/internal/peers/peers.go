// SPDX-License-Identifier: AGPL-3.0-only

// Package peers keeps the directory of the other servers of the cluster: for
// every server id (the host name registered in the shard catalog and the
// common name of its certificate) the address of its internal API.
//
// The shard catalog knows hosts and PostgreSQL ports only, so the internal
// addresses are kept here, in a file below the data directory, written by the
// leader's join and remove tasks (design control-plane-3.md, D2 and D4; the
// choice is recorded in the README). The file is public information: it holds
// no secret.
package peers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// FileName is the name of the directory file below the data directory.
const FileName = "peers.json"

// Peer is one server of the cluster.
type Peer struct {
	// ServerID is the server id: its host name in the catalog.
	ServerID string `json:"serverId"`
	// InternalAddr is host:port of its internal API.
	InternalAddr string `json:"internalAddress"`
	// Ordinal is the number the leader gave the server (0: not known yet); the
	// ids of the alerts of the server are made from it.
	Ordinal int `json:"ordinal,omitempty"`
}

// Validate checks a peer.
func (p Peer) Validate() error {
	if p.ServerID == "" || len(p.ServerID) > 253 {
		return errors.New("peers: the server id must be 1 to 253 characters")
	}
	host, port, err := net.SplitHostPort(p.InternalAddr)
	if err != nil || host == "" {
		return fmt.Errorf("peers: %q is not a host:port address", p.InternalAddr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("peers: %q has no valid port", p.InternalAddr)
	}
	return nil
}

// Directory is a file-backed set of peers, safe for concurrent use.
type Directory struct {
	path string
	mu   sync.RWMutex
	m    map[string]Peer
}

// Open reads the directory file; a missing file is an empty directory.
func Open(dataDir string) (*Directory, error) {
	d := &Directory{path: filepath.Join(dataDir, FileName), m: map[string]Peer{}}
	b, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("peers: reading %s: %w", d.path, err)
	}
	var list []Peer
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("peers: %s is not valid: %w", d.path, err)
	}
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		d.m[p.ServerID] = p
	}
	return d, nil
}

// Get returns the peer with the given id.
func (d *Directory) Get(id string) (Peer, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.m[id]
	return p, ok
}

// All returns every peer ordered by id.
func (d *Directory) All() []Peer {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Peer, 0, len(d.m))
	for _, p := range d.m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out
}

// Replace makes list the whole directory and writes it atomically.
func (d *Directory) Replace(list []Peer) error {
	m := make(map[string]Peer, len(list))
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return err
		}
		m[p.ServerID] = p
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.write(m); err != nil {
		return err
	}
	d.m = m
	return nil
}

// Put adds or replaces one peer.
func (d *Directory) Put(p Peer) error {
	if err := p.Validate(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	m := make(map[string]Peer, len(d.m)+1)
	for k, v := range d.m {
		m[k] = v
	}
	if old, ok := d.m[p.ServerID]; ok && p.Ordinal == 0 {
		p.Ordinal = old.Ordinal
	}
	m[p.ServerID] = p
	if err := d.write(m); err != nil {
		return err
	}
	d.m = m
	return nil
}

// Delete removes a peer; an unknown id is not an error.
func (d *Directory) Delete(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.m[id]; !ok {
		return nil
	}
	m := make(map[string]Peer, len(d.m))
	for k, v := range d.m {
		if k != id {
			m[k] = v
		}
	}
	if err := d.write(m); err != nil {
		return err
	}
	d.m = m
	return nil
}

func (d *Directory) write(m map[string]Peer) error {
	list := make([]Peer, 0, len(m))
	for _, p := range m {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ServerID < list[j].ServerID })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("peers: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, d.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("peers: replacing %s: %w", d.path, err)
	}
	return nil
}
