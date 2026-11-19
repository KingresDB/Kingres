// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
)

// Files serves the files of the publication to the HTTP listener (ledgerpub.Handler.Objects). On the manager leader it reads the system
// database; on a follower it asks the leader over the internal API (CallPublicationGet), with an in-process cache for the immutable
// objects (tiles, bundles, events), which never change. The listener never touches a user database.
type Files struct {
	Store ledgerpub.Store
	// Prefix returns the origin prefix in force.
	Prefix func(ctx context.Context) (string, error)
	// Leader returns the id of the manager leader and whether this server is it.
	Leader func(ctx context.Context) (id string, self bool, err error)
	// Call is the internal call to another server.
	Call func(ctx context.Context, server, method, path string, in, out any) error
	// CacheBytes bounds the cache of immutable objects (default 32 MiB; negative: no cache).
	CacheBytes int
	// Now is the clock of the prefix cache (default time.Now).
	Now func() time.Time

	mu        sync.Mutex
	prefix    string
	prefixAt  time.Time
	cache     map[string]ledgerpub.Object
	order     []string
	cacheSize int
}

func (f *Files) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// prefixCached reads the prefix at most every five seconds.
func (f *Files) prefixCached(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prefix != "" && f.now().Sub(f.prefixAt) < 5*time.Second {
		return f.prefix, nil
	}
	p, err := f.Prefix(ctx)
	if err != nil {
		return "", err
	}
	f.prefix, f.prefixAt = p, f.now()
	return p, nil
}

func pathOf(p ledgerpub.LogPath) string {
	if p.Log == "" {
		return p.Rest
	}
	return p.Log + "/" + p.Rest
}

// local reads a file from the store: this server is the leader.
func (f *Files) local(ctx context.Context, p ledgerpub.LogPath) (ledgerpub.Object, error) {
	if p.Log == "" {
		return f.Store.Object(ctx, "", p.Rest)
	}
	prefix, err := f.prefixCached(ctx)
	if err != nil {
		return ledgerpub.Object{}, err
	}
	return f.Store.Object(ctx, prefix+"/"+p.Log, p.Rest)
}

// Objects is ledgerpub.Objects.
func (f *Files) Objects(ctx context.Context, p ledgerpub.LogPath) (ledgerpub.Object, error) {
	if f.Leader == nil {
		return f.local(ctx, p)
	}
	id, self, err := f.Leader(ctx)
	if err != nil {
		return ledgerpub.Object{}, err
	}
	if self {
		return f.local(ctx, p)
	}
	key := pathOf(p)
	if o, ok := f.cached(key); ok {
		return o, nil
	}
	if f.Call == nil {
		return ledgerpub.Object{}, errors.New("ledgerround: this server cannot call the leader")
	}
	var out PublicationGetResponse
	err = f.Call(ctx, id, "POST", strings.TrimPrefix(CallPublicationGet, "POST "), PublicationGetRequest{Path: key}, &out)
	var ae *peerclient.APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return ledgerpub.Object{}, ledgerpub.ErrNotFound
	case err != nil:
		return ledgerpub.Object{}, err
	}
	o := ledgerpub.Object{Data: out.Data, Mutable: out.Mutable, ContentType: out.ContentType}
	if !o.Mutable {
		f.remember(key, o)
	}
	return o, nil
}

func (f *Files) cached(key string) (ledgerpub.Object, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.cache[key]
	return o, ok
}

func (f *Files) remember(key string, o ledgerpub.Object) {
	max := f.CacheBytes
	if max == 0 {
		max = 32 << 20
	}
	if max < 0 || len(o.Data) > max/8 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]ledgerpub.Object{}
	}
	if _, ok := f.cache[key]; ok {
		return
	}
	for f.cacheSize+len(o.Data) > max && len(f.order) > 0 {
		old := f.order[0]
		f.order = f.order[1:]
		f.cacheSize -= len(f.cache[old].Data)
		delete(f.cache, old)
	}
	f.cache[key] = o
	f.order = append(f.order, key)
	f.cacheSize += len(o.Data)
}

// Handle answers CallPublicationGet on the manager leader: the stored file, or 404. A server that is not the leader refuses (the policy
// of the call lets only members ask the leader, and the leader answers from its own store).
func (f *Files) Handle(ctx context.Context, body []byte) (any, error) {
	var req PublicationGetRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, callErr(http.StatusBadRequest, CodeBadRequest, "The body is not valid.")
	}
	p, err := ledgerpub.ParsePath(req.Path)
	if err != nil {
		return nil, callErr(http.StatusNotFound, "not_found", "No such file.")
	}
	if f.Leader != nil {
		if _, self, err := f.Leader(ctx); err != nil || !self {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_leader", Message: "This server is not the manager leader."}
		}
	}
	o, err := f.local(ctx, p)
	switch {
	case errors.Is(err, ledgerpub.ErrNotFound):
		return nil, callErr(http.StatusNotFound, "not_found", "No such file.")
	case err != nil:
		return nil, callErr(http.StatusServiceUnavailable, CodeUnavailable, "The publication cannot be read now.")
	}
	return PublicationGetResponse{Data: o.Data, Mutable: o.Mutable, ContentType: o.ContentType}, nil
}
