// SPDX-License-Identifier: AGPL-3.0-only

// Package instance manages the local PostgreSQL instances of kingresd (design
// control-plane-2.md, D3): it creates a data directory with initdb, renders
// the managed configuration and pg_hba.conf, and starts, stops, restarts and
// reloads the instance with pg_ctl, waiting for readiness.
//
// Layout below the root (<data-dir>/instances):
//
//	<id>/kingresd-instance.json   the marker: this directory is ours
//	<id>/pgdata/                  the PostgreSQL data directory
//	<id>/run/                     unix socket directory
//
// The manager refuses to act on a directory without a matching marker, never
// follows a path outside the root, and never runs as root.
package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MarkerName is the name of the marker file inside an instance directory.
const MarkerName = "kingresd-instance.json"

// Errors.
var (
	// ErrNotManaged means the directory has no valid marker of ours.
	ErrNotManaged = errors.New("instance directory is not managed by kingresd")
	// ErrExists means an instance with that id already exists.
	ErrExists = errors.New("instance already exists")
	// ErrNotRunning means the instance is not running.
	ErrNotRunning = errors.New("instance is not running")
	// ErrLeaseRequired means the instance may not be started as a primary now: the
	// start gate (SetStartGate) refused it because its group lease was lost.
	ErrLeaseRequired = errors.New("instance may not start without a lease")
)

// Marker is the content of the marker file.
type Marker struct {
	// NodeID is the id of the instance (the directory name).
	NodeID int `json:"nodeId"`
	// Port is the TCP port the instance was created for.
	Port int `json:"port"`
	// CreatedAt is the creation time.
	CreatedAt time.Time `json:"createdAt"`
	// Magic identifies the file as kingresd's.
	Magic string `json:"magic"`
}

const markerMagic = "kingresd-instance-v1"

// Options configure a Manager.
type Options struct {
	// Root is the directory that holds the instance directories; created if
	// missing.
	Root string
	// BinDir holds initdb and pg_ctl.
	BinDir string
	// SocketDir is the directory below which each instance gets a private
	// directory <id> for its unix socket (default <Root>/<id>/run). It must
	// not be in /tmp.
	SocketDir string
	// Logger receives diagnostic messages; never passwords.
	Logger *slog.Logger
	// StartTimeout and StopTimeout bound pg_ctl waits (default 90s and 60s).
	StartTimeout, StopTimeout time.Duration
	// AllowRoot disables the refusal to run as root (tests of the refusal only).
	AllowRoot bool
}

// Manager manages the instances below Root.
type Manager struct {
	root, bin, sockBase string
	log                 *slog.Logger
	startTO             time.Duration
	stopTO              time.Duration
	gate                func(id int) error
}

// New validates the options and returns a Manager.
func New(o Options) (*Manager, error) {
	if os.Geteuid() == 0 && !o.AllowRoot {
		return nil, errors.New("instance: refusing to manage PostgreSQL instances as root")
	}
	root, err := filepath.Abs(o.Root)
	if err != nil || o.Root == "" {
		return nil, invalid("root directory %q", trunc(o.Root))
	}
	bin, err := filepath.Abs(o.BinDir)
	if err != nil || o.BinDir == "" {
		return nil, invalid("PostgreSQL binary directory %q", trunc(o.BinDir))
	}
	for _, exe := range []string{"initdb", "pg_ctl"} {
		st, err := os.Stat(filepath.Join(bin, exe))
		if err != nil || st.IsDir() {
			return nil, fmt.Errorf("instance: %s not found in %s", exe, bin)
		}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("instance: creating %s: %w", root, err)
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sockBase := ""
	if o.SocketDir != "" {
		if sockBase, err = filepath.Abs(o.SocketDir); err != nil {
			return nil, invalid("socket directory %q", trunc(o.SocketDir))
		}
		if err := os.MkdirAll(sockBase, 0o750); err != nil {
			return nil, fmt.Errorf("instance: creating %s: %w", sockBase, err)
		}
	}
	m := &Manager{sockBase: sockBase, root: root, bin: bin, log: log, startTO: o.StartTimeout, stopTO: o.StopTimeout}
	if m.startTO == 0 {
		m.startTO = 90 * time.Second
	}
	if m.stopTO == 0 {
		m.stopTO = 60 * time.Second
	}
	return m, nil
}

// Dir returns the instance directory of id after checking the id.
func (m *Manager) Dir(id int) (string, error) {
	if id < 1 || id > 1<<20 {
		return "", invalid("instance id %d", id)
	}
	return filepath.Join(m.root, strconv.Itoa(id)), nil
}

// DataDir returns the PostgreSQL data directory of id.
func (m *Manager) DataDir(id int) (string, error) {
	d, err := m.Dir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "pgdata"), nil
}

// SocketDir returns the unix socket directory of id.
func (m *Manager) SocketDir(id int) (string, error) {
	d, err := m.Dir(id)
	if err != nil {
		return "", err
	}
	if m.sockBase != "" {
		return filepath.Join(m.sockBase, strconv.Itoa(id)), nil
	}
	return filepath.Join(d, "run"), nil
}

// Spec describes an instance to create.
type Spec struct {
	// ID is the node id (and directory name).
	ID int
	// Settings is the managed configuration. SocketDir is filled in by Init.
	Settings ManagedSettings
	// SuperuserPassword is the password of the role ServiceRole.
	SuperuserPassword string
	// HBA are the managed rules and TLSRequired the TLS mode.
	HBA         []HBARule
	TLSRequired bool
	// Locale is passed to initdb as --locale when not empty.
	Locale string
}

// Init creates the instance directory, runs initdb and writes the managed
// configuration and pg_hba.conf. It does not start the instance. A failure
// removes what it created.
func (m *Manager) Init(ctx context.Context, s Spec) (err error) {
	dir, err := m.Dir(s.ID)
	if err != nil {
		return err
	}
	if s.SuperuserPassword == "" || strings.ContainsAny(s.SuperuserPassword, "\r\n\x00") {
		return invalid("the superuser password must be set and single-line")
	}
	if s.Locale != "" && !validLocale(s.Locale) {
		return invalid("locale %q", trunc(s.Locale))
	}
	if _, statErr := os.Lstat(dir); statErr == nil {
		return fmt.Errorf("%w: %s", ErrExists, dir)
	}
	sock, err := m.SocketDir(s.ID)
	if err != nil {
		return err
	}
	if len(sock) > 90 {
		return invalid("socket directory path %q is too long (limit 90 bytes)", trunc(sock))
	}
	s.Settings.SocketDir = filepath.ToSlash(sock)
	// Render before touching the disk so that bad input leaves nothing behind.
	managed, err := RenderManaged(s.Settings)
	if err != nil {
		return err
	}
	hba, err := RenderHBA(s.HBA, s.TLSRequired)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("instance: creating %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			// Only our own fresh directory is removed.
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				m.log.Error("removing failed instance directory", "dir", dir, "error", rmErr)
			}
		}
	}()
	if err := os.MkdirAll(sock, 0o700); err != nil {
		return err
	}
	if m.sockBase != "" {
		defer func() {
			if err != nil {
				_ = os.RemoveAll(sock)
			}
		}()
	}
	mk, _ := json.Marshal(Marker{NodeID: s.ID, Port: s.Settings.Port, CreatedAt: time.Now().UTC(), Magic: markerMagic})
	if err := os.WriteFile(filepath.Join(dir, MarkerName), mk, 0o600); err != nil {
		return err
	}
	// The instance serves the group the catalog names for its host and port; that is not known yet.
	if err := m.WriteRole(s.ID, Role{Group: GroupUnknown, Role: RolePrimary, Origin: OriginOwn}); err != nil {
		return err
	}
	pw := filepath.Join(dir, "pwfile")
	if err := os.WriteFile(pw, []byte(s.SuperuserPassword+"\n"), 0o600); err != nil {
		return err
	}
	data := filepath.Join(dir, "pgdata")
	args := []string{"-D", data, "-U", ServiceRole, "-E", "UTF8", "--pwfile=" + pw,
		"--auth-local=scram-sha-256", "--auth-host=scram-sha-256", "--data-checksums", "--no-instructions"}
	if s.Locale != "" {
		args = append(args, "--locale="+s.Locale)
	}
	out, runErr := m.run(ctx, "initdb", args...)
	_ = os.Remove(pw) // the password does not stay on disk
	if runErr != nil {
		return fmt.Errorf("instance: initdb: %w: %s", runErr, out)
	}
	if err := os.WriteFile(filepath.Join(data, "kingres-managed.conf"), []byte(managed), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(data, ConfigFileName), []byte("# Generated by kingresd. Do not edit: changes are overwritten.\n"), 0o600); err != nil {
		return err
	}
	if err := m.saveSettings(s.ID, s.Settings); err != nil {
		return err
	}
	conf, err := os.OpenFile(filepath.Join(data, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // path below the manager root
	if err != nil {
		return err
	}
	_, werr := conf.WriteString("\n# Managed by kingresd; the settings in this file win.\ninclude 'kingres-managed.conf'\n")
	if cerr := conf.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.WriteFile(filepath.Join(data, "pg_hba.conf"), []byte(hba), 0o600)
}

func validLocale(l string) bool {
	for i := 0; i < len(l); i++ {
		c := l[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '.' && c != '-' && c != '@' {
			return false
		}
	}
	return l != "" && len(l) < 64
}

// Managed reports whether the directory of id carries our marker, and
// returns the marker.
func (m *Manager) Managed(id int) (Marker, error) {
	dir, err := m.Dir(id)
	if err != nil {
		return Marker{}, err
	}
	// The marker must be a regular file of our own (no symlink).
	p := filepath.Join(dir, MarkerName)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return Marker{}, fmt.Errorf("%w: %s", ErrNotManaged, dir)
	}
	b, err := os.ReadFile(p) //nolint:gosec // path below the manager root
	if err != nil {
		return Marker{}, fmt.Errorf("%w: %s", ErrNotManaged, dir)
	}
	var mk Marker
	if err := json.Unmarshal(b, &mk); err != nil || mk.Magic != markerMagic || mk.NodeID != id {
		return Marker{}, fmt.Errorf("%w: %s", ErrNotManaged, dir)
	}
	return mk, nil
}

// Exists reports whether instance id exists and is ours.
func (m *Manager) Exists(id int) bool {
	_, err := m.Managed(id)
	return err == nil
}

// Running reports whether the instance is running (pg_ctl status).
func (m *Manager) Running(ctx context.Context, id int) (bool, error) {
	if _, err := m.Managed(id); err != nil {
		return false, err
	}
	data, _ := m.DataDir(id)
	_, err := m.run(ctx, "pg_ctl", "status", "-D", data)
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		return false, nil
	}
	return false, fmt.Errorf("instance: pg_ctl status: %w", err)
}

// Start starts the instance and waits until it accepts connections. Starting
// a running instance is not an error.
func (m *Manager) Start(ctx context.Context, id int) error {
	unlock, err := m.lifecycle(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return m.start(ctx, id)
}

func (m *Manager) start(ctx context.Context, id int) error {
	if _, err := m.Managed(id); err != nil {
		return err
	}
	if up, err := m.Running(ctx, id); err != nil || up {
		return err
	}
	if m.gate != nil {
		if err := m.gate(id); err != nil {
			return err
		}
	}
	// The socket directory may be gone (a reboot empties /run): it is created
	// before every start, not only when the instance is created.
	sock, serr := m.SocketDir(id)
	if serr != nil {
		return serr
	}
	if err := os.MkdirAll(sock, 0o700); err != nil {
		return fmt.Errorf("instance: creating the socket directory: %w", err)
	}
	data, _ := m.DataDir(id)
	out, err := m.run(ctx, "pg_ctl", "start", "-D", data, "-w", "-t", secs(m.startTO),
		"-l", filepath.Join(data, "kingres-startup.log"))
	if err != nil {
		return fmt.Errorf("instance: pg_ctl start: %w: %s", err, out)
	}
	return nil
}

// Stop stops the instance with a fast shutdown and waits. Stopping a stopped
// instance is not an error.
func (m *Manager) Stop(ctx context.Context, id int) error {
	unlock, err := m.lifecycle(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return m.stop(ctx, id)
}

func (m *Manager) stop(ctx context.Context, id int) error {
	if up, err := m.Running(ctx, id); err != nil || !up {
		return err
	}
	data, _ := m.DataDir(id)
	out, err := m.run(ctx, "pg_ctl", "stop", "-D", data, "-m", "fast", "-w", "-t", secs(m.stopTO))
	if err != nil {
		return fmt.Errorf("instance: pg_ctl stop: %w: %s", err, out)
	}
	return nil
}

// Restart stops (if running) and starts the instance.
func (m *Manager) Restart(ctx context.Context, id int) error {
	unlock, err := m.lifecycle(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.stop(ctx, id); err != nil {
		return err
	}
	return m.start(ctx, id)
}

// lifecycleLocks serializes the starts and stops of one instance, by its data directory. `pg_ctl stop -w` waits until the
// pid file of the instance is gone: a start by another caller between the shutdown and that check
// (the standby agent starting a standby that a restart has just stopped) puts a new pid file there and the stop
// waits for the whole of its time and fails, though the instance did shut down. The shutdown paths that must not wait
// (StopImmediate, StopForShutdown) do not take the lock.
var lifecycleLocks sync.Map

// lifecycle takes the lock of an instance and returns the function that gives it back. The wait for the
// lock ends with the caller's context (ST-04): a caller that gave itself a time limit is not held up for as
// long as the holder takes, and the holder's own context is not the waiter's. The lock is a channel of
// one so that it can be waited for with a select.
func (m *Manager) lifecycle(ctx context.Context, id int) (func(), error) {
	data, _ := m.DataDir(id)
	v, _ := lifecycleLocks.LoadOrStore(data, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	default:
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("instance: waiting for another start, stop or restart of instance %d: %w", id, ctx.Err())
	}
}

// Reload asks a running instance to re-read its configuration files.
func (m *Manager) Reload(ctx context.Context, id int) error {
	up, err := m.Running(ctx, id)
	if err != nil {
		return err
	}
	if !up {
		return ErrNotRunning
	}
	data, _ := m.DataDir(id)
	if out, err := m.run(ctx, "pg_ctl", "reload", "-D", data); err != nil {
		return fmt.Errorf("instance: pg_ctl reload: %w: %s", err, out)
	}
	return nil
}

// WriteHBA validates and renders the rules and replaces pg_hba.conf
// atomically; the caller reloads the instance. On a rendering error the file
// is untouched.
func (m *Manager) WriteHBA(id int, rules []HBARule, tlsRequired bool) error {
	if _, err := m.Managed(id); err != nil {
		return err
	}
	trust, err := m.Trust(id)
	if err != nil {
		return err
	}
	text, err := RenderHBAWithNodes(rules, tlsRequired, trust)
	if err != nil {
		return err
	}
	data, _ := m.DataDir(id)
	return writeAtomic(filepath.Join(data, "pg_hba.conf"), []byte(text))
}

// WriteManaged replaces kingres-managed.conf atomically; changes of port,
// listen addresses and preloaded libraries need a restart, others a reload.
func (m *Manager) WriteManaged(id int, s ManagedSettings) error {
	mk, err := m.Managed(id)
	if err != nil {
		return err
	}
	if s.Port == 0 {
		s.Port = mk.Port
	}
	sock, _ := m.SocketDir(id)
	s.SocketDir = filepath.ToSlash(sock)
	text, err := RenderManaged(s)
	if err != nil {
		return err
	}
	data, _ := m.DataDir(id)
	if err := writeAtomic(filepath.Join(data, "kingres-managed.conf"), []byte(text)); err != nil {
		return err
	}
	return m.saveSettings(id, s)
}

// LogDir returns the directory that holds the instance's log files: the
// log_directory of its managed settings (an administrator can move it through
// updateThisServerSettings), the "log" directory of the data directory when
// none is set.
func (m *Manager) LogDir(id int) (string, error) {
	d, err := m.DataDir(id)
	if err != nil {
		return "", err
	}
	if s, err := m.Settings(id); err == nil && s.LogDirectory != "" {
		return s.LogDirectory, nil
	}
	return filepath.Join(d, "log"), nil
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func secs(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) }

// run executes a PostgreSQL program with a clean environment. Its combined
// output (bounded) is returned for error messages; no program here receives a
// password on its command line or in its environment.
func (m *Manager) run(ctx context.Context, prog string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, filepath.Join(m.bin, prog), args...) //nolint:gosec // fixed program names; arguments are built from validated values
	cmd.Env = []string{"PATH=" + m.bin + string(os.PathListSeparator) + os.Getenv("PATH"), "LC_MESSAGES=C"}
	for _, k := range []string{"LANG", "LC_ALL", "HOME", "LD_LIBRARY_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var buf bytes.Buffer
	cmd.Stdout = &limitWriter{w: &buf, n: 8 << 10}
	cmd.Stderr = cmd.Stdout
	// pg_ctl start leaves the postmaster running after it exits; do not wait
	// for pipes the postmaster inherited.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

type limitWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}
	return len(p), nil
}

// WriteIdent replaces pg_ident.conf atomically with the mapping of osUser to
// roles; the caller reloads the instance.
func (m *Manager) WriteIdent(id int, osUser string, roles []string) error {
	if _, err := m.Managed(id); err != nil {
		return err
	}
	trust, err := m.Trust(id)
	if err != nil {
		return err
	}
	text, err := RenderIdentWithNodes(osUser, roles, trust)
	if err != nil {
		return err
	}
	data, _ := m.DataDir(id)
	return writeAtomic(filepath.Join(data, "pg_ident.conf"), []byte(text))
}

// SetStartGate installs a check that runs before an instance that is not running
// is started; an error (conventionally wrapping ErrLeaseRequired) refuses the
// start. The lease manager uses it so that an instance that fenced itself is not
// started as a primary again without a lease. Call it before the first Start.
func (m *Manager) SetStartGate(f func(id int) error) { m.gate = f }

// StopImmediate stops the instance with an immediate shutdown (no checkpoint,
// recovery on the next start) and waits. It is the fence of the lease protocol:
// it must be fast and must not depend on clients disconnecting. A stopped
// instance is not an error.
func (m *Manager) StopImmediate(ctx context.Context, id int) error {
	if up, err := m.Running(ctx, id); err != nil || !up {
		return err
	}
	data, _ := m.DataDir(id)
	out, err := m.run(ctx, "pg_ctl", "stop", "-D", data, "-m", "immediate", "-w", "-t", secs(m.stopTO))
	if err != nil {
		return fmt.Errorf("instance: pg_ctl stop -m immediate: %w: %s", err, out)
	}
	return nil
}

// StopForShutdown is the orderly stop of the service: it asks the instance for a
// fast shutdown without waiting (pg_ctl stop -W), then calls requested with the
// identity of the postmaster that was asked (process id and start time, "" when
// unknown), and only then waits until the instance is gone or the stop timeout
// has passed. requested runs after PostgreSQL has accepted the request, which
// ends all sessions and admits no new one, so a watchdog told to stand down at
// that moment can no longer cost a write that was acknowledged. Stopping a
// stopped instance is not an error and calls nothing.
func (m *Manager) StopForShutdown(ctx context.Context, id int, requested func(postmaster string)) error {
	if up, err := m.Running(ctx, id); err != nil || !up {
		return err
	}
	data, _ := m.DataDir(id)
	ident := postmasterIdentity(data)
	out, err := m.run(ctx, "pg_ctl", "stop", "-D", data, "-m", "fast", "-W")
	if err != nil {
		if up, rerr := m.Running(ctx, id); rerr == nil && !up {
			return nil // it ended between the check and the request
		}
		return fmt.Errorf("instance: pg_ctl stop: %w: %s", err, out)
	}
	if requested != nil {
		requested(ident)
	}
	deadline := time.Now().Add(m.stopTO)
	for {
		up, err := m.Running(ctx, id)
		if err != nil {
			return err
		}
		if !up {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("instance: the instance did not shut down within %s", m.stopTO)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// postmasterIdentity is the process id and the start time of the postmaster, the
// first and third line of postmaster.pid ("" when unreadable); kingres-watchdog
// reads the same two lines.
func postmasterIdentity(data string) string {
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		return ""
	}
	return strings.TrimSpace(lines[0]) + " " + strings.TrimSpace(lines[2])
}
