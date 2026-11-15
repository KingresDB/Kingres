// SPDX-License-Identifier: AGPL-3.0-only

package internalapi

import "testing"

// A server asks to leave through a call that any server may make but only
// the leader answers, and that is told who asks by the certificate alone.
func TestLeaveCallsAreAuthorizedToTheLeaderForAnyPeer(t *testing.T) {
	for _, k := range []string{"POST /leave-cluster", "POST /leave-status"} {
		if p, ok := Calls[k]; !ok || p != ToLeader {
			t.Errorf("%s: policy %v (present %v), want ToLeader", k, p, ok)
		}
		if !handled[k] {
			t.Errorf("%s is not dispatched to the backend", k)
		}
	}
}

func TestLeaveCallNeedsACertificateAndTheLeaderAnswers(t *testing.T) {
	s, ca, seen := membersServer(t, false, "me") // this server is the leader
	if st := call(t, s, ca, nil, "POST", "/leave-cluster"); st != 401 {
		t.Errorf("leave without a certificate: %d", st)
	}
	peer := cert(t, ca, "db3")
	if st := call(t, s, ca, &peer, "POST", "/leave-cluster"); st != 200 {
		t.Errorf("leave by a member: %d", st)
	}
	if st := call(t, s, ca, &peer, "POST", "/leave-status"); st != 200 {
		t.Errorf("leave status by a member: %d", st)
	}
	// The backend is told the caller from the certificate, never from the body.
	want := []string{"POST /leave-cluster@db3", "POST /leave-status@db3"}
	if len(*seen) != 2 || (*seen)[0] != want[0] || (*seen)[1] != want[1] {
		t.Errorf("backend saw %v, want %v", *seen, want)
	}

	// A server that is not the leader refuses.
	s2, ca2, seen2 := membersServer(t, false, "somebody-else")
	peer2 := cert(t, ca2, "db3")
	if st := call(t, s2, ca2, &peer2, "POST", "/leave-cluster"); st != 409 {
		t.Errorf("leave sent to a server that is not the leader: %d", st)
	}
	if len(*seen2) != 0 {
		t.Errorf("a server that is not the leader ran the call: %v", *seen2)
	}
}
