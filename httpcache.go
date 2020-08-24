// Copyright 2015 Matthew Holt and The Caddy Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httpcache

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/config"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	_ = caddy.RegisterModule(Cache{})
}

// Cache implements a simple distributed cache.
//
// Caches only GET and HEAD requests. Honors the Cache-Control: no-cache header.
//
// Still TODO:
//
// - Properly set autocache options
// - Eviction policies and API
// - Use single cache per-process
// - Preserve cache through config reloads
// - More control over what gets cached
type Cache struct {
	// Maximum size of the cache, in bytes. Default is 512 MB.
	MaxSize int64 `json:"max_size,omitempty"`

	db   *olric.Olric
	dmap *olric.DMap
}

// CaddyModule returns the Caddy module information.
func (Cache) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.cache",
		New: func() caddy.Module { return new(Cache) },
	}
}

// Provision provisions c.
func (c *Cache) Provision(ctx caddy.Context) error {
	maxSize := c.MaxSize
	if maxSize == 0 {
		const maxMB = 512
		maxSize = int64(maxMB << 20)
	}

	// TODO: Set environment
	started, cancel := context.WithCancel(context.Background())
	cfg := config.New("local")
	cfg.Cache.MaxInuse = int(maxSize)
	cfg.Started = func() {
		defer cancel()
		log.Printf("[INFO] Olric is ready to accept connections")
	}
	db, err := olric.New(cfg)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		err = db.Start()
		if err != nil {
			log.Printf("[ERROR] olric.Start returned an error: %v", err)
			errCh <- err
		}
	}()
	select {
	case err = <-errCh:
		return err
	case <-started.Done():
	}
	c.dmap, err = db.NewDMap(dmapName)
	return err
}

// Validate validates c.
func (c *Cache) Validate() error {
	if c.MaxSize < 0 {
		return fmt.Errorf("size must be greater than 0")
	}
	return nil
}

func (c *Cache) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// TODO: proper RFC implementation of cache control headers...
	if r.Header.Get("Cache-Control") == "no-cache" || (r.Method != "GET" && r.Method != "HEAD") {
		return next.ServeHTTP(w, r)
	}

	getterCtx := getterContext{w, r, next}
	ctx := context.WithValue(r.Context(), getterContextCtxKey, getterCtx)

	// TODO: rigorous performance testing

	// TODO: pretty much everything else to handle the nuances of HTTP caching...

	// TODO: groupcache has no explicit cache eviction, so we need to embed
	// all information related to expiring cache entries into the key; right
	// now we just use the request URI as a proof-of-concept
	key := r.RequestURI

	value, err := c.dmap.Get(key)
	if err == olric.ErrKeyNotFound {
		// Cache the request here
		err = c.tryToCache(ctx, key)
		if err == errUncacheable {
			return nil
		}
		if err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	// the cached bytes consists of two parts: first a
	// gob encoding of the status and header, immediately
	// followed by the raw bytes of the response body
	rdr := bytes.NewReader(value.([]byte))

	// read the header and status first
	var hs headerAndStatus
	err = gob.NewDecoder(rdr).Decode(&hs)
	if err != nil {
		return err
	}

	// set and write the cached headers
	for k, v := range hs.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(hs.Status)

	// write the cached response body
	io.Copy(w, rdr)

	return nil
}

func (c *Cache) tryToCache(ctx context.Context, key string) error {
	combo := ctx.Value(getterContextCtxKey).(getterContext)

	// the buffer will store the gob-encoded header, then the body
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	// we need to record the response if we are to cache it; only cache if
	// request is successful (TODO: there's probably much more nuance needed here)
	rr := caddyhttp.NewResponseRecorder(combo.rw, buf, func(status int, header http.Header) bool {
		shouldBuf := status < 300

		if shouldBuf {
			// store the header before the body, so we can efficiently
			// and conveniently use a single buffer for both; gob
			// decoder will only read up to end of gob message, and
			// the rest will be the body, which will be written
			// implicitly for us by the recorder
			err := gob.NewEncoder(buf).Encode(headerAndStatus{
				Header: header,
				Status: status,
			})
			if err != nil {
				log.Printf("[ERROR] Encoding headers for cache entry: %v; not caching this request", err)
				return false
			}
		}

		return shouldBuf
	})

	// execute next handlers in chain
	err := combo.next.ServeHTTP(rr, combo.req)
	if err != nil {
		return err
	}

	// if response body was not buffered, response was
	// already written and we are unable to cache; or,
	// if there was no response to buffer, same thing.
	// TODO: maybe Buffered() should return false if there was no response to buffer (which would account for the case when shouldBuffer is never called)
	if !rr.Buffered() || buf.Len() == 0 {
		return errUncacheable
	}

	// add to cache
	return c.dmap.Put(key, buf.Bytes())
}

type headerAndStatus struct {
	Header http.Header
	Status int
}

type getterContext struct {
	rw   http.ResponseWriter
	req  *http.Request
	next caddyhttp.Handler
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

var errUncacheable = fmt.Errorf("uncacheable")

const dmapName = "http_requests"

type ctxKey string

const getterContextCtxKey ctxKey = "getter_context"

// Interface guards
var (
	_ caddy.Provisioner           = (*Cache)(nil)
	_ caddy.Validator             = (*Cache)(nil)
	_ caddyhttp.MiddlewareHandler = (*Cache)(nil)
)
