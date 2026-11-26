// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package tsa

import (
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
)

func fmt2hex(b []byte) string { return hex.EncodeToString(b) }

func hexDecode(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func asn1Unmarshal(b []byte, v any) ([]byte, error) { return asn1.Unmarshal(b, v) }

func pemOf(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
