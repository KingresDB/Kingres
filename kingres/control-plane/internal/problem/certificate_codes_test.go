// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package problem

import "testing"

// The certificate codes of contract 0.5.0: a dedicated 503 for an operation
// that needs the CA key when the manager leader does not hold it, and a 409 for
// a server whose certificates were revoked.
func TestCertificateCodes(t *testing.T) {
	for _, tc := range []struct {
		c      Code
		want   string
		status int
	}{
		{CertificateAuthorityUnavailable, "certificate_authority_unavailable", 503},
		{CertificateRevoked, "certificate_revoked", 409},
	} {
		p := New(tc.c, "d")
		if string(p.Code) != tc.want || p.Status != tc.status || p.Title == "" || p.Type == "" {
			t.Errorf("%s: %+v", tc.want, p)
		}
	}
}
