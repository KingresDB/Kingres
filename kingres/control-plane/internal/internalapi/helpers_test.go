// SPDX-License-Identifier: PostgreSQL

package internalapi

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
)

func newLogger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func parseLeaf(c tls.Certificate) (*x509.Certificate, error) {
	return x509.ParseCertificate(c.Certificate[0])
}
