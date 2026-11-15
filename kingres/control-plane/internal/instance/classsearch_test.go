// SPDX-License-Identifier: AGPL-3.0-only

package instance

import "testing"

// Class search of 2026-11-14: a managed setting must not be able to pull in
// another configuration file or move the files the instance reads.
func TestManagedExtraSettingsCannotIncludeOrRedirectFiles(t *testing.T) {
	for _, name := range []string{"include", "include_dir", "include_if_exists", "config_file", "ident_file", "external_pid_file", "archive_command", "ssl_passphrase_command"} {
		_, err := RenderManaged(ManagedSettings{Port: 5432, Extra: map[string]string{name: "/tmp/x"}})
		if err == nil {
			t.Errorf("setting %q was accepted", name)
		}
	}
	if _, err := RenderManaged(ManagedSettings{Port: 5432, Extra: map[string]string{"work_mem": "8MB"}}); err != nil {
		t.Errorf("an ordinary setting was refused: %v", err)
	}
}
