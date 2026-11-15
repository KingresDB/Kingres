// SPDX-License-Identifier: PostgreSQL

package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(nil, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8091" || cfg.LogLevel != "info" || cfg.SessionTTL != 8*time.Hour || cfg.MaxBodyBytes != 1<<20 {
		t.Errorf("defaults: %+v", cfg)
	}
	if want := filepath.Join(cfg.DataDir, "audit.jsonl"); cfg.AuditFile != want {
		t.Errorf("AuditFile = %q, want %q", cfg.AuditFile, want)
	}
	if !cfg.ListenIsLoopback() {
		t.Error("default listen address must be loopback")
	}
}

func TestPrecedenceFileEnvFlag(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "c.json")
	body := `{"listen": "127.0.0.1:1111", "log-level": "debug", "session-ttl": "2h", "max-body-bytes": 2048}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(map[string]string{"KINGRES_LISTEN": "127.0.0.1:2222", "KINGRES_LOG_LEVEL": "warn", "KINGRES_CONFIG": file})
	cfg, err := Load([]string{"-listen", "127.0.0.1:3333"}, env, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:3333" {
		t.Errorf("flag must win: %s", cfg.Listen)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("env must beat file: %s", cfg.LogLevel)
	}
	if cfg.SessionTTL != 2*time.Hour || cfg.MaxBodyBytes != 2048 {
		t.Errorf("file values lost: %+v", cfg)
	}
	// The -config flag beats KINGRES_CONFIG.
	other := filepath.Join(dir, "o.json")
	if err := os.WriteFile(other, []byte(`{"log-format":"json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load([]string{"-config", other}, env, &bytes.Buffer{})
	if err != nil || cfg.LogFormat != "json" || cfg.SessionTTL != 8*time.Hour {
		t.Errorf("-config: %+v %v", cfg, err)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	badKey := filepath.Join(dir, "k.json")
	_ = os.WriteFile(badKey, []byte(`{"listn":"x"}`), 0o600)
	badJSON := filepath.Join(dir, "j.json")
	_ = os.WriteFile(badJSON, []byte(`{`), 0o600)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"bad listen", []string{"-listen", "nonsense"}, nil, "listen"},
		{"bad level", []string{"-log-level", "loud"}, nil, "log-level"},
		{"bad format", nil, map[string]string{"KINGRES_LOG_FORMAT": "xml"}, "log-format"},
		{"bad duration", []string{"-session-ttl", "soon"}, nil, "duration"},
		{"zero duration", []string{"-session-ttl", "0s"}, nil, "positive"},
		{"bad int", []string{"-max-body-bytes", "-5"}, nil, "minimum"},
		{"tls half", []string{"-tls-cert-file", "c.pem"}, nil, "tls-cert-file"},
		{"unknown flag", []string{"-nope"}, nil, "nope"},
		{"positional", []string{"extra"}, nil, "unexpected argument"},
		{"unknown file key", []string{"-config", badKey}, nil, "unknown key"},
		{"bad file", []string{"-config", badJSON}, nil, "parsing config file"},
		{"missing file", []string{"-config", filepath.Join(dir, "none")}, nil, "reading config file"},
		{"empty dsn", []string{"-pg-dsn", ""}, nil, "pg-dsn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.args, envOf(tc.env), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	_, err := Load([]string{"-help"}, envOf(nil), &out)
	if !errors.Is(err, ErrHelp) || !strings.Contains(out.String(), "-pg-dsn") || !strings.Contains(out.String(), "KINGRES_PG_DSN") {
		t.Errorf("err=%v usage=%q", err, out.String())
	}
}

func TestLogValueHidesDSN(t *testing.T) {
	cfg := Defaults()
	cfg.PostgresDSN = "postgres://kingres:hunter2-secret@db:5432/postgres"
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "config", cfg)
	if strings.Contains(buf.String(), "hunter2-secret") || strings.Contains(buf.String(), "postgres://") {
		t.Errorf("DSN leaked: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "listen=127.0.0.1:8091") {
		t.Errorf("config not logged: %s", buf.String())
	}
}

func TestListenIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8091": true, "[::1]:8091": true, "localhost:8091": true,
		"0.0.0.0:8091": false, ":8091": false, "10.0.0.5:8091": false,
	} {
		c := Config{Listen: addr}
		if got := c.ListenIsLoopback(); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

func TestSlogLevel(t *testing.T) {
	for lvl, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got := (Config{LogLevel: lvl}).SlogLevel(); got != want {
			t.Errorf("%s: %v", lvl, got)
		}
	}
}

func TestCertificateSettings(t *testing.T) {
	cfg, err := Load(nil, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CertificateLifetime != 365*24*time.Hour || cfg.CertificateRenewFraction < 0.33 || cfg.CertificateRenewFraction > 0.34 ||
		cfg.CertificateCheckInterval != time.Hour || cfg.CertificateRetryInterval != time.Minute {
		t.Errorf("defaults: %v %v %v %v", cfg.CertificateLifetime, cfg.CertificateRenewFraction, cfg.CertificateCheckInterval, cfg.CertificateRetryInterval)
	}
	cfg, err = Load([]string{"-certificate-lifetime", "20s", "-certificate-renew-fraction", "0.5", "-certificate-check-interval", "250ms", "-certificate-retry-interval", "100ms"}, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CertificateLifetime != 20*time.Second || cfg.CertificateRenewFraction != 0.5 || cfg.CertificateCheckInterval != 250*time.Millisecond || cfg.CertificateRetryInterval != 100*time.Millisecond {
		t.Errorf("flags: %+v", cfg)
	}
	for _, bad := range [][]string{
		{"-certificate-lifetime", "1s"}, {"-certificate-renew-fraction", "0"}, {"-certificate-renew-fraction", "0.95"},
		{"-certificate-renew-fraction", "x"}, {"-certificate-check-interval", "0s"},
	} {
		if _, err := Load(bad, envOf(nil), &bytes.Buffer{}); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if cfg, err = Load(nil, envOf(map[string]string{"KINGRES_CERTIFICATE_LIFETIME": "90s"}), &bytes.Buffer{}); err != nil || cfg.CertificateLifetime != 90*time.Second {
		t.Errorf("environment: %v %v", cfg.CertificateLifetime, err)
	}
}

func TestMonitoringAndGatheringSettings(t *testing.T) {
	cfg, err := Load(nil, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MonitorInterval != 10*time.Second || cfg.LedgerPendingMaxAge != 5*time.Minute || cfg.GatherTimeout != 3*time.Second || cfg.VersionCheckInterval != time.Minute {
		t.Errorf("defaults: %v %v %v %v", cfg.MonitorInterval, cfg.LedgerPendingMaxAge, cfg.GatherTimeout, cfg.VersionCheckInterval)
	}
	cfg, err = Load([]string{"-monitor-interval", "500ms", "-ledger-pending-max-age", "4s", "-gather-timeout", "800ms", "-version-check-interval", "2s"}, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MonitorInterval != 500*time.Millisecond || cfg.LedgerPendingMaxAge != 4*time.Second || cfg.GatherTimeout != 800*time.Millisecond || cfg.VersionCheckInterval != 2*time.Second {
		t.Errorf("flags: %+v", cfg)
	}
	if _, err := Load([]string{"-gather-timeout", "0s"}, envOf(nil), &bytes.Buffer{}); err == nil {
		t.Error("a zero gather timeout was accepted")
	}
}

func TestPlaintextOnNetwork(t *testing.T) {
	for _, tc := range []struct {
		listen, cert string
		want         bool
	}{
		{"127.0.0.1:8091", "", false},
		{"[::1]:8091", "", false},
		{"localhost:8091", "", false},
		{"0.0.0.0:8091", "", true},
		{":8091", "", true},
		{"10.0.0.5:8091", "", true},
		{"0.0.0.0:8091", "server.pem", false},
	} {
		c := Config{Listen: tc.listen, TLSCertFile: tc.cert}
		if got := c.PlaintextOnNetwork(); got != tc.want {
			t.Errorf("listen %q cert %q: %v, want %v", tc.listen, tc.cert, got, tc.want)
		}
	}
}
