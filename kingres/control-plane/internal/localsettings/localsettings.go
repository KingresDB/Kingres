// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package localsettings implements getThisServerSettings and
// updateThisServerSettings: the settings that belong to one server (memory
// quota, log directory, log level). They are never replicated or forwarded.
//
// Where the values live:
//
//   - the memory quota and the log directory are part of the managed
//     configuration of the instance (instance.ManagedSettings, rendered into
//     kingres-managed.conf and recorded in the instance's settings file, where
//     the setup recorded the quota it was given);
//   - the log level of kingresd itself and the "restart pending" mark are in
//     <data-dir>/local-settings.json.
//
// A change of the quota or the log directory is written to the managed
// configuration and takes effect when the instance is restarted: kingresd does
// not reload it, so an instance never runs half of a change. restartRequired
// stays true until the postmaster that was running when the change was saved
// has been replaced (a persistent state, derived from postmaster.pid, so it
// survives a restart of kingresd and needs no hook on the code paths that
// restart the instance).
package localsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
)

// StateFile is the name of the state file below the data directory.
const StateFile = "local-settings.json"

// Limits of the memory quota in MiB (the schema of the contract and the
// setup's own bounds).
const (
	MinMemoryMB = 512
	MaxMemoryMB = 1 << 24
	// DefaultMemoryMB is reported for an instance created before the quota was
	// recorded and without a shared_buffers setting to derive it from.
	DefaultMemoryMB = 1024
)

// Instances is what the settings need from the instance manager.
type Instances interface {
	Settings(id int) (instance.ManagedSettings, error)
	WriteManaged(id int, s instance.ManagedSettings) error
	PostmasterToken(id int) string
}

// Options configure a Manager.
type Options struct {
	// DataDir is the data directory of kingresd (dataPaths.dataDirectory).
	DataDir string
	// Instances is the instance manager; InstanceID the instance whose
	// configuration holds the settings (1).
	Instances  Instances
	InstanceID int
	// PGPort is the client port reported as readWritePort.
	PGPort int
	// Level is the level of kingresd's logger; the saved level is applied to
	// it by New and a change is applied live.
	Level *slog.LevelVar
	// Log receives diagnostics.
	Log *slog.Logger
	// PhysicalMemoryMB reports the machine's memory in MiB, ok false when it
	// is not known (default: measured on Linux).
	PhysicalMemoryMB func() (mb int64, ok bool)
	// CheckDirectory verifies that path is an existing directory the server can
	// write to (default: the real file system).
	CheckDirectory func(path string) error
}

type state struct {
	LogLevel string `json:"logLevel,omitempty"`
	// RestartToken is the postmaster the pending change was saved under.
	RestartToken string `json:"restartToken,omitempty"`
}

// Manager reads and changes the local settings.
type Manager struct {
	o  Options
	mu sync.Mutex
	st state
}

// New loads the state file (missing means empty) and applies a saved log level.
func New(o Options) (*Manager, error) {
	if o.DataDir == "" || o.Instances == nil || o.Level == nil {
		return nil, errors.New("localsettings: data directory, instances and level are required")
	}
	if o.InstanceID == 0 {
		o.InstanceID = 1
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.PhysicalMemoryMB == nil {
		o.PhysicalMemoryMB = physicalMemoryMB
	}
	if o.CheckDirectory == nil {
		o.CheckDirectory = checkDirectory
	}
	m := &Manager{o: o}
	b, err := os.ReadFile(m.statePath()) //nolint:gosec // fixed name below the data directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("localsettings: reading %s: %w", StateFile, err)
	default:
		if err := json.Unmarshal(b, &m.st); err != nil {
			return nil, fmt.Errorf("localsettings: parsing %s: %w", StateFile, err)
		}
	}
	if lv, ok := ParseLevel(m.st.LogLevel); ok {
		o.Level.Set(lv)
	}
	return m, nil
}

func (m *Manager) statePath() string { return filepath.Join(m.o.DataDir, StateFile) }

// ParseLevel converts a contract log level to a slog level.
func ParseLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// LevelName converts a slog level to the contract's name.
func LevelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warning"
	default:
		return "error"
	}
}

// Quota returns the memory quota recorded in the managed settings, derived
// from shared_buffers for an instance created before it was recorded.
func Quota(s instance.ManagedSettings) int {
	if s.MemoryMegabytes > 0 {
		return s.MemoryMegabytes
	}
	if v, ok := s.Extra["shared_buffers"]; ok {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "MB")); err == nil && n > 0 {
			return n * 4
		}
	}
	return DefaultMemoryMB
}

// Get returns the local settings.
func (m *Manager) Get() (api.LocalSettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getLocked()
}

func (m *Manager) getLocked() (api.LocalSettings, error) {
	cur, err := m.o.Instances.Settings(m.o.InstanceID)
	if err != nil {
		return api.LocalSettings{}, fmt.Errorf("reading the managed settings: %w", err)
	}
	dataDir, err := filepath.Abs(m.o.DataDir)
	if err != nil {
		dataDir = m.o.DataDir
	}
	out := api.LocalSettings{
		DataPaths:   api.LocalDataPaths{DataDirectory: dataDir},
		MemoryQuota: api.MemoryQuota{TotalMegabytes: Quota(cur)},
		LogLevel:    LevelName(m.o.Level.Level()),
		ClientPorts: api.ClientPorts{ReadWritePort: m.o.PGPort},
	}
	if cur.LogDirectory != "" {
		d := cur.LogDirectory
		out.DataPaths.LogDirectory = &d
	}
	out.RestartRequired = m.st.RestartToken != "" && m.o.Instances.PostmasterToken(m.o.InstanceID) == m.st.RestartToken
	return out, nil
}

func fieldErr(field, msg, code string) problem.FieldError {
	return problem.FieldError{Field: field, Message: msg, Code: code}
}

// parsed is a validated request.
type parsed struct {
	quota  *int
	level  *string
	logDir *string // points to "" for null
}

// parse validates the shape of the body: members, types, enumerations and
// ranges (400 validation_failed). Whether the machine can hold the quota and
// whether the directory is usable is checked later (422).
func parse(body []byte) (parsed, *problem.Problem) {
	var p parsed
	var members map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&members); err != nil || members == nil || dec.More() {
		return p, problem.Validation("The settings update is not a JSON object.", fieldErr("/", "must be a JSON object", "type"))
	}
	var errs []problem.FieldError
	if len(members) == 0 {
		errs = append(errs, fieldErr("/", "at least one of memoryQuota, logLevel and logDirectory is required", "required"))
	}
	for k, raw := range members {
		switch k {
		case "memoryQuota":
			errs = append(errs, parseQuota(raw, &p)...)
		case "logLevel":
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				errs = append(errs, fieldErr("/logLevel", "must be a string", "type"))
				continue
			}
			if _, ok := ParseLevel(s); !ok {
				errs = append(errs, fieldErr("/logLevel", "must be debug, info, warning or error", "enum"))
				continue
			}
			p.level = &s
		case "logDirectory":
			s := ""
			if string(bytes.TrimSpace(raw)) != "null" {
				if err := json.Unmarshal(raw, &s); err != nil {
					errs = append(errs, fieldErr("/logDirectory", "must be a string or null", "type"))
					continue
				}
				if s == "" {
					errs = append(errs, fieldErr("/logDirectory", "must be an absolute path, or null for the system default", "pattern"))
					continue
				}
			}
			p.logDir = &s
		case "dataPaths", "clientPorts":
			errs = append(errs, fieldErr("/"+k, "cannot be changed after the server was created", "immutable"))
		default:
			errs = append(errs, fieldErr("/"+k, "unknown member", "unknown_property"))
		}
	}
	if len(errs) > 0 {
		sort.Slice(errs, func(i, j int) bool { return errs[i].Field < errs[j].Field })
		return p, problem.Validation("The settings update is invalid.", errs...)
	}
	return p, nil
}

func parseQuota(raw json.RawMessage, p *parsed) []problem.FieldError {
	var mq map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mq); err != nil || mq == nil {
		return []problem.FieldError{fieldErr("/memoryQuota", "must be an object", "type")}
	}
	var errs []problem.FieldError
	for qk := range mq {
		if qk != "totalMegabytes" {
			errs = append(errs, fieldErr("/memoryQuota/"+qk, "unknown member", "unknown_property"))
		}
	}
	tm, ok := mq["totalMegabytes"]
	if !ok {
		return append(errs, fieldErr("/memoryQuota/totalMegabytes", "is required", "required"))
	}
	var n int
	if err := json.Unmarshal(tm, &n); err != nil {
		return append(errs, fieldErr("/memoryQuota/totalMegabytes", "must be an integer", "type"))
	}
	if n < MinMemoryMB || n > MaxMemoryMB {
		return append(errs, fieldErr("/memoryQuota/totalMegabytes", fmt.Sprintf("must be %d to %d", MinMemoryMB, MaxMemoryMB), "out_of_range"))
	}
	p.quota = &n
	return errs
}

// Update validates and applies a PATCH body and returns the settings after the
// change. Nothing changes when any member is refused.
func (m *Manager) Update(body []byte) (api.LocalSettings, error) {
	p, perr := parse(body)
	if perr != nil {
		return api.LocalSettings{}, perr
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// 422: the machine cannot hold the quota, the directory is unusable.
	var bad []problem.FieldError
	if p.quota != nil {
		if mb, ok := m.o.PhysicalMemoryMB(); ok && int64(*p.quota) > mb {
			bad = append(bad, fieldErr("/memoryQuota/totalMegabytes", fmt.Sprintf("this machine has %d MiB of memory", mb), "exceeds_memory"))
		}
	}
	if p.logDir != nil && *p.logDir != "" {
		if msg := validateDirectoryPath(*p.logDir); msg != "" {
			bad = append(bad, fieldErr("/logDirectory", msg, "invalid_path"))
		} else if err := m.o.CheckDirectory(*p.logDir); err != nil {
			bad = append(bad, fieldErr("/logDirectory", err.Error(), "unusable"))
		}
	}
	if len(bad) > 0 {
		pr := problem.New(problem.Unprocessable, "A setting cannot be applied on this server.")
		pr.Errors = bad
		return api.LocalSettings{}, pr
	}

	cur, err := m.o.Instances.Settings(m.o.InstanceID)
	if err != nil {
		return api.LocalSettings{}, fmt.Errorf("reading the managed settings: %w", err)
	}
	next := cur
	next.Extra = map[string]string{}
	for k, v := range cur.Extra {
		next.Extra[k] = v
	}
	changed := false
	if p.quota != nil && *p.quota != Quota(cur) {
		next.MemoryMegabytes = *p.quota
		// The derived settings replace the ones the setup wrote into Extra.
		for k := range instance.MemorySettings(*p.quota) {
			delete(next.Extra, k)
		}
		changed = true
	}
	if p.logDir != nil && *p.logDir != cur.LogDirectory {
		next.LogDirectory = *p.logDir
		changed = true
	}
	newSt := m.st
	if changed {
		if err := m.o.Instances.WriteManaged(m.o.InstanceID, next); err != nil {
			return api.LocalSettings{}, fmt.Errorf("writing the managed configuration: %w", err)
		}
		// Pending until the postmaster that runs now has been replaced. A stopped
		// instance reads the file when it starts, so nothing is pending then.
		if tok := m.o.Instances.PostmasterToken(m.o.InstanceID); tok != "" {
			newSt.RestartToken = tok
		}
	}
	if p.level != nil {
		newSt.LogLevel = *p.level
	}
	if newSt != m.st {
		if err := m.saveState(newSt); err != nil {
			return api.LocalSettings{}, fmt.Errorf("saving %s: %w", StateFile, err)
		}
		m.st = newSt
	}
	if p.level != nil {
		lv, _ := ParseLevel(*p.level)
		m.o.Level.Set(lv)
	}
	return m.getLocked()
}

func (m *Manager) saveState(s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// validateDirectoryPath checks the text of a log directory before the file
// system is asked: an absolute, clean path that postgresql.conf can carry. The
// managed configuration is Linux's, so the path must start with "/".
func validateDirectoryPath(p string) string {
	switch {
	case !utf8.ValidString(p) || strings.ContainsAny(p, "\x00\r\n"):
		return "contains a control character"
	case !strings.HasPrefix(p, "/"):
		return "must be an absolute path"
	case path.Clean(p) != p:
		return "must be a clean absolute path (no .., //, or trailing /)"
	case len(p) > 1024:
		return "is too long"
	}
	return ""
}

// checkDirectory verifies that p is an existing directory in which a file can
// be created and removed by this process.
func checkDirectory(p string) error {
	st, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("does not exist")
	case err != nil:
		return errors.New("cannot be read")
	case !st.IsDir():
		return errors.New("is not a directory")
	}
	f, err := os.CreateTemp(p, ".kingres-write-test-*")
	if err != nil {
		return errors.New("is not writable by this server")
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func physicalMemoryMB() (int64, bool) {
	r := serverinfo.Resources(nil, nil)
	if r.Memory.TotalBytes == nil || *r.Memory.TotalBytes <= 0 {
		return 0, false
	}
	return *r.Memory.TotalBytes >> 20, true
}
