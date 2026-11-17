// SPDX-License-Identifier: AGPL-3.0-only

// Package tsa anchors the digest of a signed checkpoint at an RFC 3161
// time-stamp authority (design 6.4) and verifies, in full, the token that
// comes back. It has its own CMS and X.509 handling (encoding/asn1 is used
// only to build the request; every answer is read by the strict reader of
// this package, and crypto/x509 only parses certificates and builds the
// chain). It shares no code with the stand-alone verifier, which has a second
// implementation.
//
// # What it guarantees
//
// A Receipt returned by Submit holds a token that was verified before it was
// accepted: strict DER; SignedData with content type id-ct-TSTInfo and one
// SignerInfo; TSTInfo version 1 with a SHA-256 imprint equal to the digest
// sent, the nonce sent (64 random bits) and, when configured, the policy
// requested; the messageDigest attribute equal to the digest of the TSTInfo;
// the signature over the signed attributes as received; the signing
// certificate bound by the signingCertificate or signingCertificateV2
// attribute and named by the sid; its extended key usage extension critical
// and holding id-kp-timeStamping alone; a chain to the configured trust
// anchors with validity checked at genTime and the timeStamping key usage
// required of the whole chain. Nothing is stored, no goroutine outlives a
// call, the context is the timeout (Config.TimeoutSeconds, default 20, only
// bounds it further), responses are read up to MaxResponseBytes, a redirect
// is followed only to the same host, port and scheme (three hops at most)
// and the password of HTTP Basic authentication appears in no error, no log
// text and no output of fmt or encoding/json (types hold it as anchor.Secret
// or in closures; a Target prints as "tsa.Target{url}").
//
// # Evidence format
//
// Evidence is anchor.TSAEvidence, encoded by anchor.EncodeEvidence (compact
// JSON, members in this order, no trailing newline):
//
//	{"tsa_url":"<configured url>","token_der_b64":"<std base64, padded, of the token>",
//	 "gen_time":"2026-12-01T10:00:00Z","serial":"<decimal>","policy":"<dotted OID>",
//	 "recorded_by":"cluster"}
//
// token_der_b64 is the DER of the TimeStampToken, that is the ContentInfo
// (SEQUENCE { OID 1.2.840.113549.1.7.2, [0] SignedData }), byte for byte as
// the TSA sent it inside the TimeStampResp, with the certificates it carried
// (certReq is TRUE in the request). gen_time is genTime truncated to whole
// seconds, in UTC, format "2006-01-02T15:04:05Z"; serial is the TSTInfo serial
// in decimal; policy is the TSTInfo policy. Receipt.EvidenceTime is genTime
// (with its sub-seconds); Receipt.State is "anchored" at once; NextCheck is nil.
//
// A later check of stored evidence is VerifyEvidence (token as in
// VerifyStoredToken, then gen_time, serial and policy of the evidence equal
// the token's). VerifyStoredToken does not check the nonce, which is not kept.
//
// # What the caller must do
//
//   - Load the CA bundle named by Config.CABundlePath itself and hand it to
//     ParseBundle (self-signed certificates become trust anchors, the rest
//     intermediates). This package reads no file.
//   - Treat errors by their anchor class: status 3 "waiting", HTTP 5xx, 408,
//     425, 429, timeouts, cut connections and a TSA that rejects with
//     failInfo systemFailure or timeNotAvailable are retryable (RetryAfter is
//     set from the header); HTTP 401 and 403 and a rejection (status 2) are
//     ClassRefused (retried 1, 5, 15 minutes, then failed); everything that
//     is wrong with an answer (bad DER, any verification failure, status 4 and
//     5, status 0 or 1 without a token, a wrong content type) is permanent.
//     anchor.CodeOf gives a stable tag (the Code* constants and "tsa_waiting",
//     "tsa_rejected", "tsa_unavailable", "tsa_revocation", "no_token",
//     "content_type", "http_status", "transport").
//   - Raise the ledgerAnchorBehind alert when SubmitDetailed returns a
//     Warning with Code "skew" (genTime more than MaxSkew, 5 minutes, from the
//     clock). The anchor itself is kept.
//   - Make Submit idempotent per Payload.ID at the record level: every call
//     sends a new nonce and gets a new token; the first stored token wins.
//
// # What it does not do
//
// No revocation checking (no OCSP, no CRL), no long-term validation; the
// accuracy and ordering of the token are parsed and reported, not used; no
// hash algorithm other than SHA-256 for the imprint; no TSTInfo carrying a
// critical extension; no storage, retry or scheduling.
//
// # What is refused in the encoding, exactly
//
// Everything that is parsed (the TimeStampResp, the token, the TSTInfo) must
// be DER in the strict sense of checkDER: definite lengths in the shortest
// form (so no BER indefinite length), no tag number of 31 or more, SEQUENCE
// and SET constructed and every other universal type primitive (so no
// constructed OCTET STRING), BOOLEAN 00 or ff, INTEGER and ENUMERATED
// non-empty and minimal, NULL empty, at most 32 levels of nesting, no bytes
// after the outermost element. A token larger than anchor.MaxTokenDER or a
// response larger than MaxResponseBytes is refused. A SignerInfo with
// unsignedAttrs (even empty) is refused; so are two SignerInfos, a missing
// signedAttrs, a repeated contentType, messageDigest or signingCertificate
// attribute, and a SignerInfo digest algorithm that is not in the SignedData
// digestAlgorithms. Unknown signed attributes (for example signingTime) are
// ignored, as they are covered by the signature. Certificates inside the
// token are parsed by crypto/x509 (strict DER of its own); at most 16.
//
// # Choices where the design is silent or differs from an RFC
//
//   - Signature digests: SHA-256, SHA-384 and SHA-512 (the design text names
//     256 and 384). The SignerInfo digestAlgorithm decides; a signature
//     algorithm OID that names a digest must agree with it. Signature
//     algorithms: rsaEncryption and sha*WithRSAEncryption (PKCS#1 v1.5 only,
//     no PSS), id-ecPublicKey and ecdsa-with-SHA*; the key type of the
//     certificate must match. SHA-1 as the signature digest is refused.
//   - Signing certificate binding: both signingCertificate (RFC 2634, SHA-1
//     certHash) and signingCertificateV2 (RFC 5035, SHA-256 by default, 384 and
//     512 allowed) are accepted; one is required. The hash must match the
//     first ESSCertID, and the issuerSerial, when present, must carry the
//     certificate's serial number. SHA-1 here only binds the identity of a
//     certificate that is then verified by the chain.
//   - The sid is IssuerAndSerialNumber (compared byte for byte with the
//     certificate's issuer and serial) or SubjectKeyIdentifier [0].
//   - SignedData version and SignerInfo version: 1 or 3.
//   - The policy: when Config.PolicyOID is set the token's policy must equal
//     it; otherwise any policy is accepted and recorded.
//   - The TSTInfo "tsa" field is not used. A TSTInfo extension is accepted if
//     it is not critical (RFC 3161 section 2.4.2) and ignored.
//   - A token whose certificate chain reaches a configured trust anchor at
//     genTime is accepted even if the certificate has expired since.
//   - Media types: the reply must be application/timestamp-reply or
//     application/timestamp-response; any other type, or none, is refused
//     (design 6.4 step 2).
//   - A rejection (status 2) is ClassRefused except for failInfo
//     systemFailure and timeNotAvailable, which are retryable; the design
//     calls every rejection failed, but those two are transient by name.
//   - http and https TSA URLs are accepted (public authorities answer over
//     plain http and the token is self-authenticating); HTTP Basic
//     credentials are sent over whatever scheme the URL has, so use https when
//     they are set. User information inside the URL is refused.
//   - The request nonce is 64 random bits read as an unsigned integer (zero is
//     replaced by one), so it is positive and may take nine DER bytes.
//   - The trust bundle must contain at least one self-signed certificate; a
//     bundle of intermediates only is refused.
package tsa
