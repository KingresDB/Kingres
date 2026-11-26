// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgeranchor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/worm"
)

func init() {
	register(&Kind{
		Name:                anchor.KindWORM,
		CredentialsRequired: true,
		CredentialsAllowed:  true,
		Normalize:           normalizeWORM,
		Build:               buildWORM,
		Binds:               bindsWORM,
		Timeouts:            func(json.RawMessage) (time.Duration, time.Duration) { return 0, 0 },
	})
}

// bindsWORM: the key of the object ends with "<aggregate id>/<size>[.g<generation>].note" of this record, and the object is the signed note
// of the row (its SHA-256 and its length).
func bindsWORM(r Row, raw json.RawMessage) error {
	ev, err := anchor.ParseWORMEvidence(raw)
	if err != nil {
		return err
	}
	tail := strings.TrimPrefix(worm.ObjectKeyGen("p", r.AggregateID, r.Size, r.Generation), "p/")
	if ev.Key != tail && !strings.HasSuffix(ev.Key, "/"+tail) {
		return errors.New("the object key of the evidence is not the key of this checkpoint")
	}
	if len(r.Note) == 0 {
		return errors.New("the record has no note to compare the evidence with")
	}
	sum := sha256.Sum256(r.Note)
	if ev.ObjectSHA256 != hex.EncodeToString(sum[:]) || ev.ContentLength != uint64(len(r.Note)) {
		return errors.New("the object of the evidence is not the signed note of this checkpoint")
	}
	return nil
}

func normalizeWORM(raw json.RawMessage) (json.RawMessage, Info, error) {
	c, err := worm.ParseConfig(raw)
	if err != nil {
		return nil, Info{}, err
	}
	out, err := json.Marshal(c)
	if err != nil {
		return nil, Info{}, errors.New("the configuration is not valid")
	}
	info := Info{CABundle: c.CABundlePath, Warnings: c.Warnings()}
	// where the requests go: the endpoint, or the S3 host of the region; a bucket that is not path style adds the
	// virtual host of the bucket
	host, scheme := "", "https"
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return nil, Info{}, errors.New("worm: endpoint is not valid")
		}
		host, scheme = u.Host, u.Scheme
	} else if c.Region == "us-east-1" {
		host = "s3.amazonaws.com"
	} else {
		host = "s3." + c.Region + ".amazonaws.com"
	}
	port := "443"
	if scheme == "http" {
		port = "80"
	}
	hostOnly := host
	if h, p, ok := strings.Cut(host, ":"); ok && !strings.Contains(p, ":") && !strings.HasPrefix(host, "[") {
		hostOnly, port = h, p
	}
	if c.PathStyle != nil && *c.PathStyle {
		info.URLs = []string{scheme + "://" + host}
	} else {
		info.URLs = []string{scheme + "://" + c.Bucket + "." + hostOnly + ":" + port}
	}
	return out, info, nil
}

func buildWORM(env BuildEnv) (anchor.Target, error) {
	var c worm.Config
	if err := json.Unmarshal(env.Config, &c); err != nil {
		return nil, errors.New("the configuration is not valid")
	}
	if env.Creds.get("accessKeyId") == "" || env.Creds.get("secretAccessKey") == "" {
		return nil, errors.New("the credentials of the target are not set")
	}
	return worm.New(worm.Options{
		Config:          c,
		AccessKeyID:     env.Creds.get("accessKeyId"),
		SecretAccessKey: env.Creds.get("secretAccessKey"),
		SessionToken:    env.Creds.get("sessionToken"),
		Client:          env.Client,
		Clock:           env.Clock,
	})
}
