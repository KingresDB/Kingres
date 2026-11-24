// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// HAConfig holds the settings of high availability, part 1 (heartbeats, leases,
// standbys, the client entry point, the fault hook). They live in their own
// file because they belong to four independent features; every key has the
// usual flag, environment variable and file key.
type HAConfig struct {
	// HeartbeatInterval is how often this server sends a heartbeat to every other
	// server. SuspectTimeout and FailureTimeout are how long a server may be
	// silent before it is shown as suspect and as dead. LeaderLossBound is how long
	// a follower that cannot reach the leader keeps trusting its cached view for
	// decisions that need the leader.
	HeartbeatInterval time.Duration
	SuspectTimeout    time.Duration
	FailureTimeout    time.Duration
	LeaderLossBound   time.Duration

	// LeaseDuration is the validity of a lease from the start of the renewal round
	// that obtained it; LeaseRenew how often the holder renews; LeaseMargin the
	// safety margin (grantors wait it after expiry, the holder fences itself that
	// much before expiry).
	LeaseDuration time.Duration
	LeaseRenew    time.Duration
	LeaseMargin   time.Duration

	// StandbySyncGrace is how long the last streaming standby of a group may be
	// unreachable before it is removed from synchronous_standby_names.
	// SlotWarnBytes and SlotLimitBytes are the WAL a physical slot may retain
	// before an alert is raised and before the slot is dropped.
	StandbySyncGrace time.Duration
	// StandbyReplaceAfter is how long the record of an old primary that is waiting to be rebuilt (its server does not
	// answer) counts as a planned standby; after it the record is removed and the ordinary reconcile places a
	// replacement on another server (design 6.5). The alert oldPrimaryNotRebuilt comes at the shorter of ten minutes
	// and half of it.
	StandbyReplaceAfter time.Duration
	SlotWarnBytes       int64
	SlotLimitBytes      int64
	// StandbyPortBase is the first port tried for a new standby instance (0: the
	// server's pg-port plus 1000). It is kept away from pg-port + 1, which on a
	// machine that runs several servers is the next server's port that is not bound yet.
	StandbyPortBase int
	// StandbyPortEnd is the last port tried for a new standby instance (0: 499 ports from StandbyPortBase). A one-machine
	// cluster that shares the host with other servers gives each server a range of its own with the two.
	StandbyPortEnd int

	// ClientListen is the client entry point (empty: off); ClientStandbyListen the
	// optional second one that forwards to a local standby (empty: off).
	// ClientMaxConnections caps the forwarded connections of both together;
	// ClientConnectTimeout bounds connecting to the destination;
	// ClientIdleTimeout closes a forwarded connection that carried no byte in
	// either direction for that long.
	ClientListen         string
	ClientStandbyListen  string
	ClientMaxConnections int
	ClientConnectTimeout time.Duration
	ClientIdleTimeout    time.Duration

	// FaultDir enables the fault-injection hook of the tests when set: the
	// directory kingresd polls for fault files (package fault). Empty: off, and
	// nothing is read.
	FaultDir string

	// AllowNoWatchdog lets kingresd start without the lease watchdog program (kingres-watchdog). Without it a hung or dead kingresd
	// leaves a primary that lost its lease running (phase 5 review L1), so a server that runs the lease protocol refuses to start
	// without the program. The setting is for tests and for development machines only.
	AllowNoWatchdog bool
	// AdminKeysMaxAge is how old the cache of the administrators' keys may be before the declaration that a server is lost refuses it. It must be positive: there is no unbounded mode. The cache is
	// refreshed every 30 seconds while the system database runs, so its age is how long this server could not read it. 72 hours is the longest hold that the REST
	// declaration is meant to serve: beyond it the operator is expected to act on the server of the old leader (kingresd -release-map-hold), and a revoked administrator or an
	// old password is not accepted for longer than that.
	AdminKeysMaxAge time.Duration
}

// DefaultHA returns the built-in HA settings.
func DefaultHA() HAConfig {
	return HAConfig{
		HeartbeatInterval: time.Second, SuspectTimeout: 5 * time.Second, FailureTimeout: 10 * time.Second, LeaderLossBound: 30 * time.Second,
		AdminKeysMaxAge: 72 * time.Hour, LeaseDuration: 10 * time.Second, LeaseRenew: 2 * time.Second, LeaseMargin: 3 * time.Second,
		StandbySyncGrace: 30 * time.Second, StandbyReplaceAfter: 30 * time.Minute, SlotWarnBytes: 1 << 30, SlotLimitBytes: 4 << 30,
		ClientMaxConnections: 1000, ClientConnectTimeout: 5 * time.Second, ClientIdleTimeout: time.Hour,
	}
}

// EffectiveStandbyPortBase is the first port tried for a new standby instance: StandbyPortBase, or pgPort plus 1000.
func (h HAConfig) EffectiveStandbyPortBase(pgPort int) int {
	if h.StandbyPortBase != 0 {
		return h.StandbyPortBase
	}
	return pgPort + 1000
}

func (h HAConfig) validate() error {
	var errs []error
	if h.SuspectTimeout >= h.FailureTimeout {
		errs = append(errs, errors.New("suspect-timeout must be shorter than failure-timeout"))
	}
	if h.HeartbeatInterval*2 > h.SuspectTimeout {
		errs = append(errs, errors.New("heartbeat-interval must be at most half of suspect-timeout"))
	}
	if h.LeaseMargin >= h.LeaseDuration {
		errs = append(errs, errors.New("lease-margin must be shorter than lease-duration"))
	} else if h.LeaseRenew*2 > h.LeaseDuration-h.LeaseMargin {
		errs = append(errs, errors.New("lease-renew must be at most half of lease-duration minus lease-margin"))
	}
	if h.StandbyReplaceAfter <= 0 {
		errs = append(errs, errors.New("standby-replace-after must be positive"))
	}
	if h.SlotWarnBytes > h.SlotLimitBytes {
		errs = append(errs, errors.New("slot-retention-warn-bytes must not exceed slot-retention-limit-bytes"))
	}
	for _, a := range []struct{ key, v string }{{"client-listen", h.ClientListen}, {"client-standby-listen", h.ClientStandbyListen}} {
		if a.v == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(a.v); err != nil {
			errs = append(errs, fmt.Errorf("%s %q is not host:port: %w", a.key, a.v, err))
		}
	}
	if h.StandbyPortEnd != 0 && h.StandbyPortEnd < h.StandbyPortBase {
		errs = append(errs, errors.New("standby-port-end must not be below standby-port-base"))
	}
	if h.ClientListen != "" && h.ClientListen == h.ClientStandbyListen {
		errs = append(errs, errors.New("client-listen and client-standby-listen must differ"))
	}
	return errors.Join(errs...)
}

func haInt(dst func(*Config) *int, minimum int64) func(*Config, string) error {
	return func(c *Config, v string) error {
		n := int64(0)
		if err := integer(func(*Config) *int64 { return &n }, minimum)(c, v); err != nil {
			return err
		}
		*dst(c) = int(n)
		return nil
	}
}

func init() {
	settings = append(settings,
		setting{key: "heartbeat-interval", env: "KINGRES_HEARTBEAT_INTERVAL", usage: "how often this server sends a heartbeat to every other server (default 1s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.HeartbeatInterval })},
		setting{key: "suspect-timeout", env: "KINGRES_SUSPECT_TIMEOUT", usage: "silence after which a server is shown as suspect (default 5s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.SuspectTimeout })},
		setting{key: "failure-timeout", env: "KINGRES_FAILURE_TIMEOUT", usage: "silence after which a server is shown as dead (default 10s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.FailureTimeout })},
		setting{key: "leader-loss-bound", env: "KINGRES_LEADER_LOSS_BOUND", usage: "how long a server that cannot reach the leader trusts its cached view for decisions that need the leader (default 30s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.LeaderLossBound })},
		setting{key: "lease-duration", env: "KINGRES_LEASE_DURATION", usage: "validity of a group lease from the start of the renewal round (default 10s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.LeaseDuration })},
		setting{key: "lease-renew", env: "KINGRES_LEASE_RENEW", usage: "how often the holder of a group lease renews it (default 2s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.LeaseRenew })},
		setting{key: "lease-margin", env: "KINGRES_LEASE_MARGIN", usage: "safety margin of the lease protocol (default 3s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.LeaseMargin })},
		setting{key: "standby-sync-grace", env: "KINGRES_STANDBY_SYNC_GRACE", usage: "how long the last streaming standby may be unreachable before the group stops waiting for it (default 30s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.StandbySyncGrace })},
		setting{key: "standby-replace-after", env: "KINGRES_STANDBY_REPLACE_AFTER", usage: "how long an old primary that cannot be reached counts as a planned standby before a replacement is made on another server (default 30m)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.StandbyReplaceAfter })},
		setting{key: "slot-retention-warn-bytes", env: "KINGRES_SLOT_RETENTION_WARN_BYTES", usage: "WAL a standby's slot may retain before an alert is raised (default 1073741824)",
			apply: integer(func(c *Config) *int64 { return &c.HA.SlotWarnBytes }, 1)},
		setting{key: "standby-port-base", env: "KINGRES_STANDBY_PORT_BASE", usage: "first port tried for a new standby instance (default: pg-port plus 1000)",
			apply: haInt(func(c *Config) *int { return &c.HA.StandbyPortBase }, 1024)},
		setting{key: "standby-port-end", env: "KINGRES_STANDBY_PORT_END", usage: "last port tried for a new standby instance (default: 499 ports from standby-port-base)",
			apply: haInt(func(c *Config) *int { return &c.HA.StandbyPortEnd }, 1024)},
		setting{key: "slot-retention-limit-bytes", env: "KINGRES_SLOT_RETENTION_LIMIT_BYTES", usage: "WAL a standby's slot may retain before it is dropped (default 4294967296)",
			apply: integer(func(c *Config) *int64 { return &c.HA.SlotLimitBytes }, 1)},
		setting{key: "admin-keys-max-age", env: "KINGRES_ADMIN_KEYS_MAX_AGE", usage: "how old the cache of the administrators' keys may be before a declaration that a server is lost refuses it (default 72h; must be positive, there is no unbounded mode)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.AdminKeysMaxAge })},
		setting{key: "allow-no-watchdog", env: "KINGRES_ALLOW_NO_WATCHDOG", usage: "true lets kingresd start without the lease watchdog program kingres-watchdog (default false; for tests and development only: a hung kingresd then leaves a primary that lost its lease running)",
			apply: func(c *Config, v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("invalid boolean %q", v)
				}
				c.HA.AllowNoWatchdog = b
				return nil
			}},
		setting{key: "client-listen", env: "KINGRES_CLIENT_LISTEN", usage: "host:port of the client entry point that forwards PostgreSQL connections to the group primary (empty: off)",
			apply: str(func(c *Config) *string { return &c.HA.ClientListen })},
		setting{key: "client-standby-listen", env: "KINGRES_CLIENT_STANDBY_LISTEN", usage: "host:port of the entry point that forwards to a local standby for reads (empty: off)",
			apply: str(func(c *Config) *string { return &c.HA.ClientStandbyListen })},
		setting{key: "client-max-connections", env: "KINGRES_CLIENT_MAX_CONNECTIONS", usage: "cap on the connections the client entry points forward at once (default 1000)",
			apply: haInt(func(c *Config) *int { return &c.HA.ClientMaxConnections }, 1)},
		setting{key: "client-connect-timeout", env: "KINGRES_CLIENT_CONNECT_TIMEOUT", usage: "time the client entry point gets to connect to the destination (default 5s)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.ClientConnectTimeout })},
		setting{key: "client-idle-timeout", env: "KINGRES_CLIENT_IDLE_TIMEOUT", usage: "a forwarded connection without traffic for this long is closed (default 1h)",
			apply: dur(func(c *Config) *time.Duration { return &c.HA.ClientIdleTimeout })},
		setting{key: "fault-dir", env: "KINGRES_FAULT_DIR", usage: "TESTS ONLY: directory polled for fault-injection files (empty: off)",
			apply: str(func(c *Config) *string { return &c.HA.FaultDir })},
	)
}
