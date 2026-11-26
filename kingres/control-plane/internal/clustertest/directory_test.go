// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
)

// A server that joins is "done" for itself as soon as the leader has told it to
// finish; the leader's task still has to send the peer directory. When the next
// server is introduced in that window the finishing task used to replace the whole
// directory with a list read before the introduction, so the new server's task
// could not find the address of the server it was adding: step 1 waited for ten
// minutes, or step 2 failed with "no internal address is known". The fault holds
// the finishing task between reading the list and writing it, which makes the
// window as wide as the test needs instead of waiting for a loaded machine.
func TestServerIntroducedWhileAnotherAddFinishesIsNotLostFromTheDirectory(t *testing.T) {
	c := clustertest.New(t, 3)
	tokens := make([]string, 3)
	for i, nd := range c.Nodes {
		nd.Start()
		tokens[i] = nd.SetupToken()
	}
	leader := c.Leader()
	leader.InitCluster("directory")
	leader.SetFaults(fault.Spec{DirectoryDelayMs: 8000})

	if st, body := c.Nodes[1].JoinFrom(leader, ""); st != 202 {
		t.Fatalf("join: %d %v", st, body)
	}
	c.Nodes[1].WaitSetup(8 * time.Minute) // the leader's task still waits to write the directory
	st, body := leader.AddServer(c.Nodes[2], tokens[2], "")
	if st != 202 {
		t.Fatalf("add: %d %v", st, body)
	}
	done := leader.WaitTask(clustertest.TaskID(body), 3*time.Minute)
	if done["state"] != "done" {
		t.Fatalf("the add of the third server ended %v: %v\n%v", done["state"], done["error"], done["progress"])
	}
	leader.Heal()
	for i, nd := range c.Nodes {
		if !nd.Login(clustertest.AdminUser, clustertest.AdminPass) {
			t.Fatalf("no sign-in on node %d", i)
		}
	}
	if got := leader.Servers(); len(got) != 3 {
		t.Errorf("the leader lists %v", got)
	}
}

// A server that joined says it is a member only when its own service connection
// works: it signs a session in with the role's credentials, but every request after
// that re-checks the role with the service role of the instance the join created.
// Declaring the state "clustered" before those settings were applied made the first
// leader-bound read through a server that had just joined (again) answer 503. The
// fault holds the end of the join for longer than a request waits for the instance.
func TestServerThatJoinedAnswersRoleChecksAsSoonAsItIsAMember(t *testing.T) {
	c := clustertest.New(t, 2)
	for _, nd := range c.Nodes {
		nd.Start()
	}
	leader, joiner := c.Leader(), c.Nodes[1]
	leader.InitCluster("finish")
	joiner.SetFaults(fault.Spec{FinishDelayMs: 14000})
	if st, body := joiner.JoinFrom(leader, ""); st != 202 {
		t.Fatalf("join: %d %v", st, body)
	}
	joiner.WaitSetup(8 * time.Minute)
	if !joiner.Login(clustertest.AdminUser, clustertest.AdminPass) {
		t.Fatal("no sign-in on the server that joined")
	}
	if st, out := joiner.Do("GET", "/server-groups", nil, nil); st != 200 {
		t.Errorf("a leader-bound read through the server that just joined: %d %v\n%s\n%s", st, out, leader.LogTail(2500), joiner.LogTail(2500))
	}
	if st, out := joiner.Do("GET", "/roles", nil, nil); st != 200 {
		t.Errorf("a local read through the server that just joined: %d %v", st, out)
	}
}
