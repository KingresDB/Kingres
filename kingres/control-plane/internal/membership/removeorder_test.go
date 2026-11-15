// SPDX-License-Identifier: AGPL-3.0-only

package membership

import "testing"

// CP2-14: the certificate is revoked before the step of the removal that cannot
// be undone, so that a failure at the revocation never leaves a server that is
// gone and still holds a valid certificate.
func TestRemovalRevokesBeforeTheIrreversibleStep(t *testing.T) {
	steps := (&Manager{}).removeDefinition().Steps
	revoke, irreversible := -1, -1
	for i, s := range steps {
		switch {
		case s.Irreversible:
			irreversible = i
		case s.Name == "Revoke the certificate and distribute the revocation list":
			revoke = i
		}
	}
	if revoke < 0 || irreversible < 0 || revoke > irreversible {
		t.Fatalf("revocation at step %d, irreversible step at %d", revoke, irreversible)
	}
}

// Unconfirmed item of the second review: server ids that differ only by case
// are the same host to a certificate check, so they must not be two members.
func TestCaseCollision(t *testing.T) {
	existing := []string{"node1", "10.0.0.2"}
	for id, want := range map[string]bool{"Node1": true, "NODE1": true, "node1": false, "node2": false, "10.0.0.2": false} {
		if got := caseCollision(existing, id); got != want {
			t.Errorf("caseCollision(%q) = %v, want %v", id, got, want)
		}
	}
}
