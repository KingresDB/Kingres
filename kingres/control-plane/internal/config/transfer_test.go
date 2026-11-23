// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

// The data transfer settings: defaults below the data directory, flags and environment, and the key file outside the staging directory.
func TestTransferSettings(t *testing.T) {
	cfg, err := Load([]string{"-data-dir", "d"}, envOf(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TransferDir != filepath.Join("d", "transfer") || cfg.TransferKeyFile != filepath.Join("d", "transfer.key") ||
		cfg.TransferMaxDuration != 24*time.Hour || cfg.MaxAtomicBytes != 2<<30 {
		t.Errorf("defaults: %+v", cfg)
	}
	cfg, err = Load([]string{"-transfer-dir", "t", "-transfer-key-file", "k", "-transfer-max-duration", "2h", "-max-atomic-bytes", "4096"}, envOf(nil), &bytes.Buffer{})
	if err != nil || cfg.TransferDir != "t" || cfg.TransferKeyFile != "k" || cfg.TransferMaxDuration != 2*time.Hour || cfg.MaxAtomicBytes != 4096 {
		t.Errorf("flags: %+v %v", cfg, err)
	}
	cfg, err = Load(nil, envOf(map[string]string{"KINGRES_TRANSFER_DIR": "e"}), &bytes.Buffer{})
	if err != nil || cfg.TransferDir != "e" {
		t.Errorf("env: %+v %v", cfg, err)
	}
	for _, args := range [][]string{
		{"-transfer-dir", "t", "-transfer-key-file", filepath.Join("t", "key")},
		{"-transfer-dir", "t", "-transfer-key-file", filepath.Join("t", "sub", "..", "key")},
		{"-max-atomic-bytes", "0"},
		{"-transfer-max-duration", "0s"},
	} {
		if _, err := Load(args, envOf(nil), &bytes.Buffer{}); err == nil {
			t.Errorf("%v must be refused", args)
		}
	}
	// A key file next to the directory, with a name that starts like it, is fine.
	if _, err := Load([]string{"-transfer-dir", "t", "-transfer-key-file", "t.key"}, envOf(nil), &bytes.Buffer{}); err != nil {
		t.Errorf("t.key beside t: %v", err)
	}
}
