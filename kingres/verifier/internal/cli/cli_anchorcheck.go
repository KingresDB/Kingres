// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchorev"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
)

var targetIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkEvidenceFiles reads and checks the evidence files ids of the anchored checkpoint of size n of the aggregate log ol, as
// the command anchors does: the digest is the one recomputed from the signed log, the evidence is never believed. It is shared by
// anchors and bundle verify. The state says what verified and what could not be checked.
func (e *env) checkEvidenceFiles(r *result, p *pubInputs, ol *openedLog, aggID string, n uint64, rel string, digest [32]byte, ids []string, roots *x509.CertPool, wormTo map[string]string, wormAll string, headers map[uint64][]byte) (*anchorsState, outcome) {
	st := &anchorsState{verified: map[string]int{}}
	for _, id := range ids {
		raw, rerr := p.src.ReadFile(rel+"/"+id+".json", anchorev.MaxFileSize)
		if rerr != nil {
			o := pubOutcome(classifyPub(rel+"/"+id+".json", rerr))
			if o.kind == outIncomplete {
				st.unver = append(st.unver, o.missing...)
				continue
			}
			return st, o
		}
		f, perr := anchorev.Parse(raw)
		if perr != nil {
			return st, failedOut("anchor-evidence", fmt.Errorf("%s: %w", id, perr))
		}
		if f.Origin != ol.cp.Origin || f.Size != n {
			return st, failedOut("anchor-evidence", fmt.Errorf("%s: the evidence is for the checkpoint %q of size %d and this is %q of size %d", id, f.Origin, f.Size, ol.cp.Origin, n))
		}
		if f.PayloadSHA256 != digest {
			return st, failedOut("anchor-evidence", fmt.Errorf("%s: payload_sha256 is %x and the digest of the checkpoint, recomputed from the signed log, is %x: the evidence stamps something else", id, f.PayloadSHA256[:8], digest[:8]))
		}
		var o outcome
		switch f.Kind {
		case anchorev.KindWORM:
			u := wormTo[id]
			if u == "" {
				u = wormAll
			}
			o = e.checkWORM(r, p, st, id, f, aggID, digest, n, u)
		case anchorev.KindTSA:
			o = checkTSA(r, st, id, f, digest, roots)
		case anchorev.KindOTS:
			o = checkOTS(r, st, id, f, digest, headers)
		}
		if !o.ok() {
			return st, o
		}
	}
	return st, outcome{}
}

// evidenceIDs lists the target ids that have an evidence file under rel (a/<id>/anchors/<size>): the explicit ones, or the
// files of the directory when the source can list. found is false when the directory is absent.
func evidenceIDs(p *pubInputs, rel string, explicit []string) (ids []string, found bool, o outcome) {
	names := map[string]bool{}
	for _, t := range explicit {
		names[t] = true
	}
	if len(names) == 0 {
		files, lerr := p.src.Files(rel)
		switch {
		case lerr == nil:
			for _, f := range files {
				if t, ok := strings.CutSuffix(f, ".json"); ok && targetIDRE.MatchString(t) {
					names[t] = true
				}
			}
		case errors.Is(lerr, pubsrc.ErrNoListing):
			return nil, false, inputOut(errors.New("this source cannot list directories: name the evidence files with --target ID"))
		case absent(lerr):
		default:
			return nil, false, pubOutcome(classifyPub(rel, lerr))
		}
	}
	for t := range names {
		ids = append(ids, t)
	}
	sort.Strings(ids)
	return ids, len(ids) > 0, outcome{}
}

// anchorMeans are the means the auditor gave to verify anchors: where to re-fetch a WORM object, and Bitcoin headers.
type anchorMeans struct {
	wormTo  map[string]string
	wormAll string
	headers map[uint64][]byte
}

func parseAnchorMeans(wormURLs, headers []string) (*anchorMeans, error) {
	m := &anchorMeans{headers: map[uint64][]byte{}}
	for _, h := range headers {
		g := btcHeaderRE.FindStringSubmatch(h)
		if g == nil {
			return nil, fmt.Errorf("--btc-header %q is not HEIGHT:<160 hex digits>", h)
		}
		n, _ := strconv.ParseUint(g[1], 10, 64)
		raw, _ := hex.DecodeString(g[2])
		if old, ok := m.headers[n]; ok && string(old) != string(raw) {
			return nil, fmt.Errorf("--btc-header: two different headers for height %d", n)
		}
		m.headers[n] = raw
	}
	var err error
	if m.wormTo, m.wormAll, err = parseWormURLs(wormURLs); err != nil {
		return nil, err
	}
	return m, nil
}

var anchorPathRE = regexp.MustCompile(`^a/([0-9a-f]{64})/anchors/(0|[1-9][0-9]{0,17})/([A-Za-z0-9_-]{1,64})\.json$`)

// bundleAnchors verifies the anchors of every aggregate log of a bundle. The sizes and targets come from the directories of the
// source when it can list them, and from the paths bundle.json names (hints, never believed) when it cannot. A failure or an input
// error is the outcome; what could not be checked is returned as unver, for the caller to count.
func (e *env) bundleAnchors(r *result, p *pubInputs, aggLogs []*openedLog, listed []string, m *anchorMeans) (verified int, unver []string, o outcome) {
	roots, rerr := tsaRoots(p.base.TSARoots)
	if rerr != nil {
		return 0, nil, inputOut(rerr)
	}
	hint := map[string]map[uint64][]string{} // aggregate id -> size -> targets named by the index
	for _, path := range listed {
		g := anchorPathRE.FindStringSubmatch(path)
		if g == nil {
			unver = append(unver, fmt.Sprintf("bundle.json lists the anchor file %q, which is not a path of the layout (a/<id>/anchors/<size>/<target>.json): it was not read", path))
			continue
		}
		n, _ := strconv.ParseUint(g[2], 10, 62)
		if hint[g[1]] == nil {
			hint[g[1]] = map[uint64][]string{}
		}
		hint[g[1]][n] = append(hint[g[1]][n], g[3])
	}
	have := map[string]bool{}
	for _, ol := range aggLogs {
		id := ol.in.spec.id
		have[id] = true
		sizes := map[uint64]bool{}
		for n := range hint[id] {
			sizes[n] = true
		}
		dirs, _, derr := p.src.SubDirs("a/" + id + "/anchors")
		if derr != nil && !absent(derr) && !errors.Is(derr, pubsrc.ErrNoListing) {
			return verified, unver, pubOutcome(classifyPub("a/"+id+"/anchors", derr))
		}
		for _, d := range dirs {
			if n, err := strconv.ParseUint(d, 10, 62); err == nil && strconv.FormatUint(n, 10) == d {
				sizes[n] = true
			}
		}
		order := make([]uint64, 0, len(sizes))
		for n := range sizes {
			order = append(order, n)
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		for _, n := range order {
			if n == 0 || n > ol.cp.Size {
				unver = append(unver, fmt.Sprintf("anchors of the aggregate log %s for size %d: the signed checkpoint of the bundle has size %d, so the digest of that checkpoint cannot be recomputed and the anchor was not checked", id[:8], n, ol.cp.Size))
				continue
			}
			text, out := anchoredText(ol, n)
			if !out.ok() {
				if out.kind == outIncomplete {
					unver = append(unver, out.missing...)
					continue
				}
				return verified, unver, out
			}
			digest := sha256.Sum256(text)
			rel := fmt.Sprintf("a/%s/anchors/%d", id, n)
			ids, _, out := evidenceIDs(p, rel, hint[id][n])
			if out.kind == outInput {
				unver = append(unver, fmt.Sprintf("the evidence files under %s could not be listed and bundle.json names none: %v", rel, out.err))
				continue
			}
			if !out.ok() {
				if out.kind == outIncomplete {
					unver = append(unver, out.missing...)
					continue
				}
				return verified, unver, out
			}
			if len(ids) == 0 {
				unver = append(unver, fmt.Sprintf("no evidence file was found under %s", rel))
				continue
			}
			st, out := e.checkEvidenceFiles(r, p, ol, id, n, rel, digest, ids, roots, m.wormTo, m.wormAll, m.headers)
			if !out.ok() {
				if out.kind == outIncomplete {
					unver = append(unver, out.missing...)
				} else {
					return verified, unver, out
				}
			}
			unver = append(unver, st.unver...)
			for _, c := range st.verified {
				verified += c
			}
		}
	}
	for id := range hint {
		if !have[id] {
			unver = append(unver, fmt.Sprintf("bundle.json lists anchors of the aggregate log %s, which is not a verified log of the bundle: they were not checked", id[:8]))
		}
	}
	return verified, unver, outcome{}
}
