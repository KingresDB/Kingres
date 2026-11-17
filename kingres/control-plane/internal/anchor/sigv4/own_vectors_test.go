// SPDX-License-Identifier: AGPL-3.0-only

package sigv4

// Vectors of our own for the properties that the AWS suite tests when it is
// present (path normalisation, escaping, query order, header folding and
// order, session token, S3 paths that are not normalised). They were
// produced once by an independent implementation of the algorithm written from
// the AWS documentation (a short program in another language that shares no
// code with this package); the "vanilla" case gives the signature that the
// AWS suite documents for its get-vanilla request. They are part of the test
// binary, so these properties are tested even where testdata/third_party is
// absent.

import (
	"testing"
	"time"
)

type ownVector struct {
	name, method, path, query string
	headers                   []Header
	body                      string
	normalize                 bool
	region, service           string
	creq, sts, authz          string
}

var ownVectors = []ownVector{
	{name: "vanilla", method: "GET", path: "/", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{name: "dot-segments", method: "GET", path: "/a/./b/../c//d/", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "GET\n/a/c/d/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nabc63ef1de4540ae41d34381aba59d5be460e9900dd733d43c6da2f28051a2cb",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=07fc47830311f547240f25f97c1565bd7a705059e39e5d0c3e0c5287c2c4bcfc"},
	{name: "empty-path", method: "GET", path: "", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
	{name: "special-path", method: "GET", path: "/ /%/~/_-./\xc3\xbc/a+b", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "GET\n/%20/%25/~/_-./%C3%BC/a%2Bb\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\nbbc9cc617ae0d3a3f7a590e8989773356c36b59d87e12e15c9ff588aae49465f",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5b445c8b7a8ef82760b0ac90fd5056bff46fe52a4c0721da0065d779658c1e68"},
	{name: "query", method: "GET", path: "/", query: "b=2&a=3&a=1&c=&d&%7e=%2B&Z=%C3%BC", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "GET\n/\nZ=%C3%BC&a=1&a=3&b=2&c=&d=&~=%2B\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\na37af9349fa8c3389d8fbdf691ad63a32478aae13fdf92f58cdcd210bdbb4084",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=21096b97141b3a9275fb435b6239b543891f356d1774459746d91fddb64d79d6"},
	{name: "headers", method: "POST", path: "/", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}, {"My-Header1", "  a   b   c  "}, {"UPPER", "v"}, {"My-Header1", "x"}, {"X-Amz-Trailing", "\x09t\x09"}}, body: "", normalize: true, region: "us-east-1", service: "service",
		creq:  "POST\n/\n\nhost:example.amazonaws.com\nmy-header1:a b c,x\nupper:v\nx-amz-date:20150830T123600Z\nx-amz-trailing:t\n\nhost;my-header1;upper;x-amz-date;x-amz-trailing\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n88bcc9bfc36b98004c5f0080ff0f9c709840ec624c7a0b1ea49862ff845d4446",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;my-header1;upper;x-amz-date;x-amz-trailing, Signature=6b1f3d105299bc5148fb80f4c72ce10d148fa5bb6e1516cc32a9f71506ecfeb0"},
	{name: "post-body", method: "POST", path: "/", query: "", headers: []Header{{"Host", "example.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}, {"Content-Type", "application/x-www-form-urlencoded; charset=utf-8"}}, body: "Param1=value1", normalize: true, region: "us-east-1", service: "service",
		creq:  "POST\n/\n\ncontent-type:application/x-www-form-urlencoded; charset=utf-8\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n32031df15172a0c1541fd8f995b6351948c6a4b045b8c592e4d1b59299ed3a29",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=2f3b42f35f135abf9c562afcbbc44fc03df96dcfd4332ecebad8b39a7d4b6125"},
	{name: "s3-keeps-path", method: "PUT", path: "/bucket/a//b/../c d/\xc3\xbc.note", query: "versionId=3%2Fa%20b", headers: []Header{{"Host", "s3.eu-test-1.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}, {"X-Amz-Content-Sha256", "1c727d26215adccb96d725e8b63b3ee11cf73215a554e60295877244b0778847"}, {"Content-Type", "text/plain; charset=utf-8"}, {"If-None-Match", "*"}, {"X-Amz-Object-Lock-Mode", "COMPLIANCE"}}, body: "note body", normalize: false, region: "eu-test-1", service: "s3",
		creq:  "PUT\n/bucket/a//b/../c%20d/%C3%BC.note\nversionId=3%2Fa%20b\ncontent-type:text/plain; charset=utf-8\nhost:s3.eu-test-1.amazonaws.com\nif-none-match:*\nx-amz-content-sha256:1c727d26215adccb96d725e8b63b3ee11cf73215a554e60295877244b0778847\nx-amz-date:20150830T123600Z\nx-amz-object-lock-mode:COMPLIANCE\n\ncontent-type;host;if-none-match;x-amz-content-sha256;x-amz-date;x-amz-object-lock-mode\n1c727d26215adccb96d725e8b63b3ee11cf73215a554e60295877244b0778847",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/eu-test-1/s3/aws4_request\nbe361e81f0d9630ad9a40fc3ba78a4cc71103253c7fbd870d90771474ab55b85",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/eu-test-1/s3/aws4_request, SignedHeaders=content-type;host;if-none-match;x-amz-content-sha256;x-amz-date;x-amz-object-lock-mode, Signature=3c5df8c3fbe30f60dde7f47e6e9632de1c43d08c304820a486f4c4dafdde8f52"},
	{name: "session-token", method: "GET", path: "/", query: "lifecycle", headers: []Header{{"Host", "examplebucket.s3.amazonaws.com"}, {"X-Amz-Date", "20150830T123600Z"}, {"X-Amz-Security-Token", "FQoGZXIvYXdzEJr//////////wEaDExampleSessionToken+/=="}}, body: "", normalize: false, region: "us-east-1", service: "s3",
		creq:  "GET\n/\nlifecycle=\nhost:examplebucket.s3.amazonaws.com\nx-amz-date:20150830T123600Z\nx-amz-security-token:FQoGZXIvYXdzEJr//////////wEaDExampleSessionToken+/==\n\nhost;x-amz-date;x-amz-security-token\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		sts:   "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/s3/aws4_request\n0cc9a916c80a407e3f6f8d4251bf69e4103a98de94c078067c8f3ddeac48c50d",
		authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=544f90543aa6f5b9efe2e2f9f01b16a91f61d21e464dbbd960bb88b4dd57a540"},
}

func TestOwnVectors(t *testing.T) {
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	const secret, keyID = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "AKIDEXAMPLE"
	for _, v := range ownVectors {
		t.Run(v.name, func(t *testing.T) {
			in := Input{Method: v.method, Path: v.path, RawQuery: v.query, Headers: v.headers, PayloadHash: HashPayload([]byte(v.body))}
			creq, signed := Canonical(in, v.normalize)
			if creq != v.creq {
				t.Fatalf("canonical request\n got: %q\nwant: %q", creq, v.creq)
			}
			scope := Scope(at, v.region, v.service)
			sts := StringToSign(at, scope, creq)
			if sts != v.sts {
				t.Fatalf("string to sign\n got: %q\nwant: %q", sts, v.sts)
			}
			got := Authorization(keyID, scope, signed, Signature(SigningKey(secret, at, v.region, v.service), sts))
			if got != v.authz {
				t.Fatalf("authorization\n got: %q\nwant: %q", got, v.authz)
			}
		})
	}
	if len(ownVectors) < 9 {
		t.Fatal("vectors missing")
	}
}
