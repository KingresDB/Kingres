// SPDX-License-Identifier: PostgreSQL

// Package config parses the configuration of kingresd from, in increasing
// order of precedence: built-in defaults, an optional JSON config file,
// environment variables (KINGRES_*) and command-line flags.
//
// Every setting is declared once in the settings table, which drives the
// flag set, the environment lookup and the config file keys, so the three
// sources cannot drift apart.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration of kingresd. It is parsed
// once in main and passed to constructors; nothing reads it globally.
type Config struct {
	// Listen is the host:port the REST API and the panel listen on.
	Listen string
	// PostgresDSN is the connection string of the local map-group instance.
	// It may contain a password and must never be logged.
	PostgresDSN string
	// DataDir holds the node state, the setup token and the audit log.
	DataDir string
	// AuditFile is the path of the JSON-lines audit log; empty means
	// <DataDir>/audit.jsonl.
	AuditFile string
	// LogLevel is debug, info, warn or error.
	LogLevel string
	// LogFormat is text or json.
	LogFormat string
	// SessionTTL is the absolute lifetime of a session and of a token.
	SessionTTL time.Duration
	// MaxSessions bounds the number of live sessions.
	MaxSessions int
	// ReadHeaderTimeout bounds reading the request headers.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds reading a whole request.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing a response.
	WriteTimeout time.Duration
	// IdleTimeout bounds an idle keep-alive connection.
	IdleTimeout time.Duration
	// RequestTimeout bounds the processing time of one API request.
	RequestTimeout time.Duration
	// ShutdownTimeout bounds the graceful shutdown.
	ShutdownTimeout time.Duration
	// MaxBodyBytes is the largest accepted request body.
	MaxBodyBytes int64
	// LoginRateLimit is the number of login attempts per client address and
	// per user name in LoginRateWindow.
	LoginRateLimit int
	// LoginRateWindow is the window of LoginRateLimit.
	LoginRateWindow time.Duration
	// TLSCertFile and TLSKeyFile enable HTTPS when both are set.
	TLSCertFile string
	TLSKeyFile  string
	// PGBinDir is the directory with the PostgreSQL binaries; when set,
	// kingresd manages the local instance (initdb, pg_ctl).
	PGBinDir string
	// PGPort and PGListen are the port and listen_addresses of the managed
	// instance.
	PGPort   int
	PGListen string
	// SocketDir is the base directory of the instances' unix sockets;
	// empty means <DataDir>/run.
	SocketDir string
	// AllowRemoteSetup lets setup operations come from non-loopback
	// addresses while the node is uninitialized (default false: safe).
	AllowRemoteSetup bool
	// MetricsToken is the optional scrape token of /metrics.
	MetricsToken string
	// StopInstances is not a setting: it is set by the -stop-instances mode.
	// ServerHost is the host name this server registers in the cluster
	// catalog; it is the server id. Empty means the machine's host name.
	ServerHost string
}

// setting describes one configuration key.
type setting struct {
	// key is the config file key and, with dashes, the flag name.
	key string
	// env is the environment variable name.
	env string
	// usage is the flag help text.
	usage string
	// secret hides the default and value in help and errors.
	secret bool
	// apply parses a string value into the config.
	apply func(c *Config, v string) error
}

func str(dst func(*Config) *string) func(*Config, string) error {
	return func(c *Config, v string) error { *dst(c) = v; return nil }
}

func dur(dst func(*Config) *time.Duration) func(*Config, string) error {
	return func(c *Config, v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", v, err)
		}
		if d <= 0 {
			return fmt.Errorf("duration must be positive, got %q", v)
		}
		*dst(c) = d
		return nil
	}
}

func integer(dst func(*Config) *int64, minimum int64) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer %q: %w", v, err)
		}
		if n < minimum {
			return fmt.Errorf("value %d is below the minimum %d", n, minimum)
		}
		*dst(c) = n
		return nil
	}
}

var settings = []setting{
	{key: "listen", env: "KINGRES_LISTEN", usage: "listen address (host:port) of the REST API and panel",
		apply: str(func(c *Config) *string { return &c.Listen })},
	{key: "pg-dsn", env: "KINGRES_PG_DSN", secret: true,
		usage: "PostgreSQL connection string of the local map-group instance (may contain a password)",
		apply: str(func(c *Config) *string { return &c.PostgresDSN })},
	{key: "data-dir", env: "KINGRES_DATA_DIR", usage: "directory for node state, setup token and audit log",
		apply: str(func(c *Config) *string { return &c.DataDir })},
	{key: "audit-file", env: "KINGRES_AUDIT_FILE", usage: "audit log file (default <data-dir>/audit.jsonl)",
		apply: str(func(c *Config) *string { return &c.AuditFile })},
	{key: "log-level", env: "KINGRES_LOG_LEVEL", usage: "log level: debug, info, warn or error",
		apply: str(func(c *Config) *string { return &c.LogLevel })},
	{key: "log-format", env: "KINGRES_LOG_FORMAT", usage: "log format: text or json",
		apply: str(func(c *Config) *string { return &c.LogFormat })},
	{key: "session-ttl", env: "KINGRES_SESSION_TTL", usage: "absolute lifetime of sessions and tokens",
		apply: dur(func(c *Config) *time.Duration { return &c.SessionTTL })},
	{key: "max-sessions", env: "KINGRES_MAX_SESSIONS", usage: "maximum number of live sessions",
		apply: func(c *Config, v string) error {
			n := int64(0)
			if err := integer(func(*Config) *int64 { return &n }, 1)(c, v); err != nil {
				return err
			}
			c.MaxSessions = int(n)
			return nil
		}},
	{key: "read-header-timeout", env: "KINGRES_READ_HEADER_TIMEOUT", usage: "HTTP read header timeout",
		apply: dur(func(c *Config) *time.Duration { return &c.ReadHeaderTimeout })},
	{key: "read-timeout", env: "KINGRES_READ_TIMEOUT", usage: "HTTP read timeout",
		apply: dur(func(c *Config) *time.Duration { return &c.ReadTimeout })},
	{key: "write-timeout", env: "KINGRES_WRITE_TIMEOUT", usage: "HTTP write timeout",
		apply: dur(func(c *Config) *time.Duration { return &c.WriteTimeout })},
	{key: "idle-timeout", env: "KINGRES_IDLE_TIMEOUT", usage: "HTTP idle timeout",
		apply: dur(func(c *Config) *time.Duration { return &c.IdleTimeout })},
	{key: "request-timeout", env: "KINGRES_REQUEST_TIMEOUT", usage: "processing timeout of one API request",
		apply: dur(func(c *Config) *time.Duration { return &c.RequestTimeout })},
	{key: "shutdown-timeout", env: "KINGRES_SHUTDOWN_TIMEOUT", usage: "graceful shutdown timeout",
		apply: dur(func(c *Config) *time.Duration { return &c.ShutdownTimeout })},
	{key: "max-body-bytes", env: "KINGRES_MAX_BODY_BYTES", usage: "maximum request body size in bytes",
		apply: integer(func(c *Config) *int64 { return &c.MaxBodyBytes }, 1)},
	{key: "login-rate-limit", env: "KINGRES_LOGIN_RATE_LIMIT", usage: "login attempts per client address and user name per window",
		apply: func(c *Config, v string) error {
			n := int64(0)
			if err := integer(func(*Config) *int64 { return &n }, 1)(c, v); err != nil {
				return err
			}
			c.LoginRateLimit = int(n)
			return nil
		}},
	{key: "login-rate-window", env: "KINGRES_LOGIN_RATE_WINDOW", usage: "window of the login rate limit",
		apply: dur(func(c *Config) *time.Duration { return &c.LoginRateWindow })},
	{key: "tls-cert-file", env: "KINGRES_TLS_CERT_FILE", usage: "TLS certificate file (enables HTTPS with tls-key-file)",
		apply: str(func(c *Config) *string { return &c.TLSCertFile })},
	{key: "tls-key-file", env: "KINGRES_TLS_KEY_FILE", usage: "TLS private key file",
		apply: str(func(c *Config) *string { return &c.TLSKeyFile })},
	{key: "pg-bin-dir", env: "KINGRES_PG_BIN_DIR", usage: "directory with the PostgreSQL binaries; enables managing the local instance",
		apply: str(func(c *Config) *string { return &c.PGBinDir })},
	{key: "pg-port", env: "KINGRES_PG_PORT", usage: "TCP port of the managed instance",
		apply: func(c *Config, v string) error {
			n := int64(0)
			if err := integer(func(*Config) *int64 { return &n }, 1024)(c, v); err != nil {
				return err
			}
			if n > 65535 {
				return fmt.Errorf("port %d is above 65535", n)
			}
			c.PGPort = int(n)
			return nil
		}},
	{key: "pg-listen", env: "KINGRES_PG_LISTEN", usage: "listen_addresses of the managed instance",
		apply: str(func(c *Config) *string { return &c.PGListen })},
	{key: "socket-dir", env: "KINGRES_SOCKET_DIR", usage: "base directory of the instances' unix sockets (default <data-dir>/run; never /tmp)",
		apply: str(func(c *Config) *string { return &c.SocketDir })},
	{key: "allow-remote-setup", env: "KINGRES_ALLOW_REMOTE_SETUP", usage: "true lets the setup operations be called from non-local addresses while the node is uninitialized (default false)",
		apply: func(c *Config, v string) error {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("invalid boolean %q", v)
			}
			c.AllowRemoteSetup = b
			return nil
		}},
	{key: "metrics-token", env: "KINGRES_METRICS_TOKEN", secret: true, usage: "bearer token that may scrape /metrics (default none: a session token of a kingres_stats account is required)",
		apply: str(func(c *Config) *string { return &c.MetricsToken })},
	{key: "server-host", env: "KINGRES_SERVER_HOST", usage: "host name this server registers in the cluster catalog (default: the machine's host name)",
		apply: str(func(c *Config) *string { return &c.ServerHost })},
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Listen:            "127.0.0.1:8091",
		PostgresDSN:       "host=127.0.0.1 port=5432 dbname=postgres",
		DataDir:           "kingresd-data",
		LogLevel:          "info",
		LogFormat:         "text",
		SessionTTL:        8 * time.Hour,
		MaxSessions:       10000,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		RequestTimeout:    30 * time.Second,
		ShutdownTimeout:   15 * time.Second,
		MaxBodyBytes:      1 << 20,
		LoginRateLimit:    10,
		LoginRateWindow:   time.Minute,
		PGPort:            5432,
		PGListen:          "127.0.0.1",
	}
}

// ErrHelp is returned by Load when -help was requested; the usage text has
// already been written to the output.
var ErrHelp = flag.ErrHelp

// Env is the source of environment variables (os.LookupEnv in production).
type Env func(key string) (string, bool)

// Load builds the configuration. args are the command-line arguments without
// the program name, env looks up environment variables, and usageOut receives
// the usage text for -help and flag errors.
func Load(args []string, env Env, usageOut io.Writer) (Config, error) {
	cfg := Defaults()

	fs := flag.NewFlagSet("kingresd", flag.ContinueOnError)
	fs.SetOutput(usageOut)
	configFile := fs.String("config", "", "path of a JSON config file (env KINGRES_CONFIG)")
	flagVals := make(map[string]*string, len(settings))
	for _, s := range settings {
		flagVals[s.key] = fs.String(s.key, "", s.usage+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	path := *configFile
	if !set["config"] {
		if v, ok := env("KINGRES_CONFIG"); ok {
			path = v
		}
	}
	if path != "" {
		if err := applyFile(&cfg, path); err != nil {
			return Config{}, err
		}
	}
	for _, s := range settings {
		if v, ok := env(s.env); ok && v != "" {
			if err := s.apply(&cfg, v); err != nil {
				return Config{}, fmt.Errorf("environment %s: %w", s.env, err)
			}
		}
	}
	for _, s := range settings {
		if set[s.key] {
			if err := s.apply(&cfg, *flagVals[s.key]); err != nil {
				return Config{}, fmt.Errorf("flag -%s: %w", s.key, err)
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyFile reads a JSON object whose keys are the setting keys.
func applyFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator configuration
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing config file %s: %w", path, err)
	}
	byKey := make(map[string]setting, len(settings))
	for _, s := range settings {
		byKey[s.key] = s
	}
	for k, rawVal := range raw {
		s, ok := byKey[k]
		if !ok {
			return fmt.Errorf("config file %s: unknown key %q", path, k)
		}
		var v string
		if err := json.Unmarshal(rawVal, &v); err != nil {
			// Numbers and other scalars are accepted in their JSON text form.
			v = strings.TrimSpace(string(rawVal))
		}
		if err := s.apply(cfg, v); err != nil {
			return fmt.Errorf("config file %s: key %q: %w", path, k, err)
		}
	}
	return nil
}

// Validate checks cross-field constraints and fills derived defaults.
func (c *Config) Validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("listen %q is not host:port: %w", c.Listen, err))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log-level %q must be debug, info, warn or error", c.LogLevel))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log-format %q must be text or json", c.LogFormat))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data-dir must not be empty"))
	}
	if c.PostgresDSN == "" {
		errs = append(errs, errors.New("pg-dsn must not be empty"))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("tls-cert-file and tls-key-file must be set together"))
	}
	if c.SocketDir == "" {
		c.SocketDir = filepath.Join(c.DataDir, "run")
	}
	if c.AuditFile == "" {
		c.AuditFile = filepath.Join(c.DataDir, "audit.jsonl")
	}
	return errors.Join(errs...)
}

// SlogLevel converts LogLevel to a slog.Level.
func (c Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LogValue implements slog.LogValuer and omits the DSN so that the
// configuration can be logged without leaking a password.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen", c.Listen),
		slog.String("data_dir", c.DataDir),
		slog.String("audit_file", c.AuditFile),
		slog.String("log_level", c.LogLevel),
		slog.String("log_format", c.LogFormat),
		slog.Duration("session_ttl", c.SessionTTL),
		slog.Bool("tls", c.TLSCertFile != ""),
	)
}

// ListenIsLoopback reports whether the listen address only accepts local
// connections.
func (c Config) ListenIsLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
