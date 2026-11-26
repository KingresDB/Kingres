// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package sigv4 implements AWS Signature Version 4 with signing by header, for
// the requests that package worm makes to an S3 compatible store (PutObject,
// GetObject, HeadObject, GetBucketVersioning, GetObjectLockConfiguration). It
// uses the standard library only.
//
// # What it does
//
// Signer.Sign sets X-Amz-Date, X-Amz-Security-Token (when a session token is
// given), X-Amz-Content-Sha256 (unless omitted) and Authorization on an
// *http.Request. The canonical request, the string to sign and the signing key
// are exactly those of the AWS specification:
//
//   - the canonical URI is the decoded path percent-encoded once, every byte
//     except A-Z a-z 0-9 - _ . ~ and "/" as %XX with upper case hex; for S3
//     (NormalizePath false) it is not normalised, so dot segments and repeated
//     slashes are kept as sent; with NormalizePath true (the rule of the
//     generic services, used to run the published test suite) dot segments
//     and repeated slashes are removed;
//   - the canonical query is the query decoded ("+" is a space), every name
//     and value encoded as above, sorted by name and then by value; a name
//     without "=" has an empty value; empty parameters (between two "&") are
//     dropped;
//   - the canonical headers are all headers of the request (Host from
//     req.Host or the URL) except those the HTTP stack manages (Authorization,
//     User-Agent, Accept-Encoding, Connection, Content-Length,
//     Transfer-Encoding, Expect, and a few others), names lower case, values
//     trimmed with runs of white space folded to one space, repeated headers
//     joined by "," in the order given;
//   - the payload hash is the one the caller passes (lower case hex SHA-256
//     of the body, or UNSIGNED-PAYLOAD).
//
// Headers added to the request after Sign are not covered by the signature.
//
// # Evidence of correctness
//
// The tests run the cases of the published "AWS Signature Version 4 test
// suite" (as kept in the botocore project, testdata/SOURCES.txt) through
// Canonical, StringToSign, SigningKey, Signature and Authorization, and four
// examples of the Amazon S3 documentation through Sign on a real request. The
// worm tests additionally verify every request with a separate implementation.
// The generic suite does not cover S3's single encoding and missing path
// normalisation; those follow the S3 documentation.
//
// # Third-party test data
//
// The published suite is third-party data (Apache-2.0, from the botocore
// project). It is not part of the tree: testdata/fetch-third-party.sh fetches
// it from a pinned commit into testdata/third_party and verifies it against
// testdata/third_party.sha256. It is test data only, read at run time by the
// tests, never embedded and never part of a binary. Without it the tests that
// need it skip and every other test runs. The properties it tests (path
// normalisation, escaping, query order, header folding, the session token, S3
// paths that are not normalised) are also tested with vectors of our own
// (own_vectors_test.go), produced once by an independent implementation of
// the algorithm; they are part of the test binary. See testdata/SOURCES.txt.
//
// # What it does not do
//
// Query-string (presigned) signing, chunked and trailing-checksum payload
// signing, double encoding of the path (the rule of the services other than
// S3), and credential discovery. Credentials are of type anchor.Secret and
// print as a placeholder in every format and in JSON.
package sigv4
