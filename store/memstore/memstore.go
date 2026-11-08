// Package memstore is the in-memory reference store (STORE-4): every record
// in maps, for a daemon whose state need not outlive its process.
//
// It meets every semantic of [store.Store] for the process's lifetime. It
// serializes commits trivially, so passing the stale-owner tests against it
// is not evidence of fencing (TEST-6, ADR 0005 item 11). Queued triggers
// are lost when the process exits (MODE-4).
package memstore

import (
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/internal/storecore"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// Option configures a memory store.
type Option func(*storecore.Options)

// WithClock sets the store's clock, used for leases and timestamps. The
// default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(o *storecore.Options) { o.Now = now }
}

// WithPauseHook sets the pause hook (ADR 0005, item 6). Test-only: it lets
// the conformance suite run against this store; production code never sets
// it.
func WithPauseHook(h store.PauseHook) Option {
	return func(o *storecore.Options) { o.Pause = h }
}

// New returns an empty memory store.
func New(opts ...Option) store.Store {
	var o storecore.Options
	for _, opt := range opts {
		opt(&o)
	}
	return storecore.New(newBackend(), o)
}

// backend holds files in a map, with each directory's entries indexed so
// listing does not scan every file. The engine serializes all access.
type backend struct {
	files    map[string][]byte
	children map[string]map[string]struct{}
}

func newBackend() *backend {
	return &backend{files: map[string][]byte{}, children: map[string]map[string]struct{}{}}
}

func (b *backend) Read(path string) ([]byte, bool, error) {
	data, ok := b.files[path]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), data...), true, nil
}

func (b *backend) List(dir string) ([]string, error) {
	names := make([]string, 0, len(b.children[dir]))
	for name := range b.children[dir] {
		names = append(names, name)
	}
	return names, nil
}

func (b *backend) Apply(writes []storecore.Write) error {
	for _, w := range writes {
		if w.Delete {
			delete(b.files, w.Path)
			if dir, name, ok := cut(w.Path); ok {
				delete(b.children[dir], name)
			}
			continue
		}
		b.files[w.Path] = append([]byte(nil), w.Data...)
		for p := w.Path; ; {
			dir, name, ok := cut(p)
			if !ok {
				break
			}
			if b.children[dir] == nil {
				b.children[dir] = map[string]struct{}{}
			}
			b.children[dir][name] = struct{}{}
			p = dir
		}
	}
	return nil
}

func (*backend) Close() error { return nil }

// cut splits a path into its directory and its last component; a path with
// no directory is listed under "".
func cut(path string) (dir, name string, ok bool) {
	if path == "" {
		return "", "", false
	}
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return "", path, true
	}
	return path[:i], path[i+1:], true
}
