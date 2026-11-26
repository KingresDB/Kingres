// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestStandbyPortRange(t *testing.T) {
	cfg, err := Load([]string{"-standby-port-base", "28000", "-standby-port-end", "28024"}, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HA.StandbyPortBase != 28000 || cfg.HA.StandbyPortEnd != 28024 {
		t.Fatalf("%d %d", cfg.HA.StandbyPortBase, cfg.HA.StandbyPortEnd)
	}
	cfg, err = Load(nil, envOf(map[string]string{"KINGRES_STANDBY_PORT_END": "30000"}), &bytes.Buffer{})
	if err != nil || cfg.HA.StandbyPortEnd != 30000 {
		t.Fatalf("the environment: %d %v", cfg.HA.StandbyPortEnd, err)
	}
	if cfg, err = Load(nil, envOf(nil), &bytes.Buffer{}); err != nil || cfg.HA.StandbyPortEnd != 0 {
		t.Fatalf("the default is 0 (499 ports from the base): %d %v", cfg.HA.StandbyPortEnd, err)
	}
	_, err = Load([]string{"-standby-port-base", "28000", "-standby-port-end", "27999"}, envOf(nil), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "standby-port-end") {
		t.Fatalf("an end below the base is refused: %v", err)
	}
}

func TestStandbyPortEndIsCheckedAgainstTheEffectiveBase(t *testing.T) {
	// No base: the first port is pg-port plus 1000 (5432 + 1000 = 6432 by default).
	if _, err := Load([]string{"-standby-port-end", "6000"}, envOf(nil), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "standby-port-end") {
		t.Fatalf("an end below pg-port plus 1000 is refused: %v", err)
	}
	if _, err := Load([]string{"-standby-port-end", "6432"}, envOf(nil), &bytes.Buffer{}); err != nil {
		t.Fatalf("the end may equal the first port: %v", err)
	}
	if _, err := Load([]string{"-pg-port", "20000", "-standby-port-end", "21000"}, envOf(nil), &bytes.Buffer{}); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := Load([]string{"-pg-port", "20000", "-standby-port-end", "20999"}, envOf(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("an end below the other server's pg-port plus 1000 is refused")
	}
}
