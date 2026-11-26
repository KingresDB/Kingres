// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
// required of the whole chain; an RSA signer key of at least 2048 bits; the
// ESSCertID issuer, when present, the certificate's issuer; the TSTInfo tsa
// name, when present, a name of the certificate; the key usage of the signer,
// when present, allowing a signature. Submit also bounds the time against the
// clock (below). Nothing is stored, no goroutine outlives a
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
//	 "signer_key_sha256":"<64 hex>","recorded_by":"cluster"}
//
// token_der_b64 is the DER of the TimeStampToken, that is the ContentInfo
// (SEQUENCE { OID 1.2.840.113549.1.7.2, [0] SignedData }), byte for byte as
// the TSA sent it inside the TimeStampResp, with the certificates it carried
// (certReq is TRUE in the request). gen_time is genTime truncated to whole
// seconds, in UTC, format "2006-01-02T15:04:05Z"; serial is the TSTInfo serial
// in decimal; policy is the TSTInfo policy; signer_key_sha256 is the lower case hex
// SHA-256 of the DER SubjectPublicKeyInfo of the signing certificate (what
// SignerKeyHash computes, correction 97). Receipt.EvidenceTime is genTime
// (with its sub-seconds); Receipt.State is "anchored" at once; NextCheck is nil.
// The URL has no query and no user information (the file is public). The token
// is accepted by Submit only if genTime is within Config.MaxSkewSeconds (default
// 300) of the clock in either direction (code "time_skew", class refused) and
// the signing certificate chains to the roots at the clock as well as at
// genTime (code "chain_not_trusted_now", class refused): a token dated inside
// the validity window of a certificate that has expired is never recorded as
// anchored now. This is applied by Submit, the method of the interface, and not
// by VerifyStoredToken, because stored evidence is judged at genTime alone (the
// right rule for evidence years later).
//
// A later check of stored evidence is VerifyEvidence (token as in
// VerifyStoredToken, then gen_time, serial and policy of the evidence equal
// the token's). VerifyStoredToken does not check the nonce, which is not kept.
// What a verifier written from this text must reproduce: the digest the token
// stamps is SHA-256 of the checkpoint text (payload_sha256 of the file, not in
// the evidence); the chain is built to the verifier's own roots with the
// validity checked at genTime and not at the verifier's clock; the signer
// certificate carries id-kp-timeStamping alone, critical. The tokens in stored
// evidence are strict DER (the cluster stored only tokens that passed that
// reader), so a verifier that also reads BER reads them all; it should not
// accept a new token that is BER. tsa_url is informational and not part of
// what is signed. The certificates carried by the token are not covered by any
// signature except the signer's, which is bound by its ESSCertID hash: the
// stored bytes of an intermediate or a root that the token carries are not
// authenticated, which is why the roots are the verifier's own.
//
// # The pin of the signer (corrections 92 and 97)
//
// Config.SignerKeySHA256 (JSON "signerKeySha256", 64 lower case hexadecimal
// digits) optionally pins the signer: the SHA-256 of the DER
// SubjectPublicKeyInfo of the signing certificate, which SignerKeyHash computes
// (openssl: x509 -pubkey -noout, pkey -pubin -outform der, dgst -sha256). The
// public key is pinned and not the certificate, because an authority renews its
// certificate more often than its key. A token whose signer has another key is
// refused with Code "signer_not_pinned" (permanent) even when it chains to the
// roots. Without a pin any authority under the configured roots is accepted and
// the name in the token is only checked against the token's own certificate:
// an operator who means one authority says so with the pin. Expect.SignerKeySHA256
// is the same pin for VerifyToken. VerifyStoredToken and VerifyEvidence take the
// pin as an argument (nil: no pin), so that an auditor who means one authority
// applies it to stored evidence; VerifyEvidence also checks that the evidence
// carries the hash of the key that signed the token. When several certificates of
// a token match the sid of its SignerInfo (an older certificate of the same key
// id or issuer and serial listed before the one that signed), every one is tried
// as the signer and the first that passes all the checks is the signer; when none
// does, the error is the first one's (which may be that of a decoy certificate, not
// of the right one).
//
// The hash that the evidence carries (signer_key_sha256) is NOT a pin: it is
// checked against the signer key of the token inside the same evidence, so it adds
// no fixed point from outside. With no pin, anyone who holds a token from any
// authority under the roots can write evidence that passes VerifyEvidence, with
// the matching hash. Only the pin argument is a value from outside; the field is
// useful to an indexer that does not parse the token, and as the value an operator
// copies into the configuration (correction 106).
//
// # What the caller must do
//
//   - Know that the name check can refuse an honest authority, and that the
//     refusal is permanent (A4-08, correction 97): the TSTInfo "tsa" name is
//     compared with the names of the signing certificate by the narrow profile
//     below, so a certificate whose PrintableString holds a character outside
//     the PrintableString set (some real certificates carry an ampersand or an
//     at sign), or whose name is the same text in another Unicode normalisation
//     (composed against decomposed letters), or in another string type than the
//     "tsa" field, is refused with Code "tsa_name_mismatch" on every attempt: a
//     Retry gets the same refusal. The operator then asks the
//     authority to issue tokens whose "tsa" field is byte for byte a name of its
//     certificate (or to leave the field out, which RFC 3161 allows), or uses
//     another authority; there is no switch that turns the check off, and the pin
//     (Config.SignerKeySHA256) does not replace it.
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
//     "content_type", "http_status", "transport"). The alert text must come
//     from the code and the text of the error, not from the class: "time_skew"
//     and "chain_not_trusted_now" are of class refused (retried 1, 5, 15
//     minutes, then failed) and are problems of clocks and of the authority's
//     certificate, not of credentials, and their text says so and says what to
//     do.
//   - Raise ledgerAnchorBehind from the age of the newest anchor, not from the
//     token: a token whose genTime is more than the tolerance from the clock is
//     not recorded at all (time_skew).
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
//   - A reply of another media type (a captive portal, a proxy page, an error
//     page that answers 200) is retryable, and a token in BER (indefinite length
//     or a constructed OCTET STRING, which some authorities emit) is refused for
//     ever as bad_der: use another authority or ask the operator of this one for
//     DER.
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
//   - The TSTInfo "tsa" field, when present, must correspond to a name of the
//     signing certificate (RFC 3161 2.4.2): the subject or a directoryName of
//     the subject alternative name, compared by the decoded attribute values
//     and not by the bytes, with a narrow profile (correction 92): a value that
//     is not valid in its string type (invalid UTF-8, a lone surrogate, an odd
//     length, a byte outside the type) is compared by its bytes only and never
//     equals another value; the letters A to Z are folded to lower case and no
//     other character is (the Kelvin sign is not a k); leading and trailing
//     ASCII spaces are removed and runs of ASCII spaces made one, and no other
//     character is white space (a no-break space, a tab and an ideographic space
//     stay what they are; a value of only spaces is one space, not empty); a
//     TeletexString is decoded only when every byte is ASCII and is otherwise
//     compared by its bytes (T.61 is not Latin-1); a DNS name (ASCII case
//     ignored), URI, e-mail
//     address or IP address of the alternative names; or an otherName that is
//     one of them byte for byte. The token is refused (tsa_name_mismatch) only
//     when no name of the certificate corresponds.
//     A TSTInfo extension is accepted if it is not critical and ignored.
//   - The RSA key of the signer must have 2048 bits or more (the floor of the
//     toolchain would let 1024 pass, and a GODEBUG setting lower still).
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
//     credentials over http:// are refused unless Config.AllowInsecure is set.
//     User information and a query inside the URL are refused.
//   - The request nonce is 64 random bits read as an unsigned integer (zero is
//     replaced by one), so it is positive and may take nine DER bytes.
//   - The trust bundle must contain at least one self-signed certificate; a
//     bundle of intermediates only is refused.
package tsa
