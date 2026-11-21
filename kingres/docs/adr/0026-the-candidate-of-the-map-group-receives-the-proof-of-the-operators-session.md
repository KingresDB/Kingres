<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# ADR 0026: The candidate of a map group failover receives the proof of the operator's session

- Status: Proposed
- Date: 2026-11-21
- Source: [../design/ha-phase5-part2.md](../design/ha-phase5-part2.md) (3.4, F4-05) and the independent review of failover round 2 (F5-05)

## Context

A proof of a session (`internal/fwdproof`) lets the receiver recover the user's ClientKey, which is as good as the password for that user. The rule so far was that a proof goes to the manager leader only: the leader holds every verifier and runs every cluster operation, and the calls that go to every member carry an assertion signed by the leader instead.

The failover of the map group is the case where the leader is the server that is gone. An operator's request reaches a member, and the server that promotes the map group is the candidate: a member that hosts a ready standby of the map group. Before F4-05 the member sent the request with the decision `allowDataLoss` and the author in a plain body, and any member could set them. F4-05 closed that by having the member sign the request with the session's proof key and the candidate verify it against the verifier it holds in its copy of the system database. The proof therefore goes to a server that is not the leader, which the earlier text said would never happen.

## Decision

1. The signed request of an operator for the failover of the map group is sent to the candidate, and the candidate may learn the operator's ClientKey from it. This is accepted for this one call.
2. Why it is acceptable: the candidate is the next leader if the bootstrap succeeds (it is promoted to hold the system database, the task engine and the certificate escrow), it already holds a full copy of the system database with every verifier, and it is chosen among the servers that host a standby of the map group. A server that could not be trusted with the operator's ClientKey could not be trusted with the cluster after the promotion either. The exposure exists even when the bootstrap is refused, and it ends when the user's password changes (the verifier changes, the key with it).
3. The plain form of `map/start` stays refused for an operator's request: nothing but a proof carries the decision `allowDataLoss` or the author. The plain form of the planned switchover (the old leader's own task) carries no decision.
4. A session of the token kind has no proof key. When the candidate is another server, `failoverServer` for the map group answers `503 service_unavailable` with a Retry-After for such a session, and the operation text says so; the operator signs in with a password session or sends the request to the candidate.
5. The documents say the same: internal-api.md ("Forwarded identities need a proof" and the `map/start` row), the README (CP2-01 row and the first item of the trust list) and the package comment of `fwdproof`.

## Consequences

- One more server can recover an operator's ClientKey, for the one call that cannot go through the leader. An operator who is concerned changes the password after a failover of the map group; the next sign-in makes a new key.
- No contract change. The behaviour for a token session is documented in the operation text of `failoverServer`.

## Alternatives rejected

- **Relaying through the leader.** The leader is the server that is gone.
- **A token that the leader signed after it verified the session.** The same reason, and a token signed before the loss would have to be asked for before the operator knows that it is needed.
- **A signature that does not reveal a key equivalent to the password.** Not possible with the SCRAM-style proof; a different scheme (a signature key per session, registered at sign-in with every server) would change the sign-in for every request to remove the exposure of one call.
- **An operator-confirmed one-time token on the candidate's own HTTP API.** It needs the operator to reach the candidate and to sign in there, which is the case that a failover of the map group does not wait for.
