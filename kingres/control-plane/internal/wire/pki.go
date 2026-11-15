// SPDX-License-Identifier: PostgreSQL

package wire

import "time"

// RenewRequest is the call POST /pki/renew: a server asks the manager leader,
// which holds the cluster CA, for a new certificate. The server that renews is
// the one named by the caller's certificate; the request carries only the
// certificate request, made from the key the server already holds (the key
// never leaves the server).
type RenewRequest struct {
	CSR string `json:"csr"`
}

// RenewResponse is the new certificate, with the CA certificate and the
// current revocation list so that the server is up to date afterwards.
type RenewResponse struct {
	CACert     string    `json:"caCert"`
	ServerCert string    `json:"serverCert"`
	Serial     string    `json:"serial"`
	NotAfter   time.Time `json:"notAfter"`
	Revoked    []string  `json:"revoked"`
	CRL        string    `json:"crl"`
}

// CSRResponse is the answer of POST /pki/csr: a certificate request for the
// server's own names, made from the key it holds.
type CSRResponse struct {
	CSR string `json:"csr"`
}
