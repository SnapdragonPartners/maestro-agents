package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/internal/regularfile"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// inboxCache remembers each inbox file's SHA-256 by name, size and
// modification time, so listing the inbox does not hash every file on
// every page.
type inboxCache struct {
	m  map[string]inboxEntry
	mu sync.Mutex
}

type inboxEntry struct {
	mod  time.Time
	sha  string
	size int64
}

func (c *inboxCache) hash(path string, fi os.FileInfo) (string, error) {
	c.mu.Lock()
	if e, ok := c.m[path]; ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		c.mu.Unlock()
		return e.sha, nil
	}
	c.mu.Unlock()
	f, err := regularfile.Open(path, true)
	if err != nil {
		return "", err //nolint:wrapcheck // names the file
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err //nolint:wrapcheck // names the file
	}
	sha := hex.EncodeToString(h.Sum(nil))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]inboxEntry{}
	}
	c.m[path] = inboxEntry{sha: sha, size: fi.Size(), mod: fi.ModTime()}
	return sha, nil
}

// inboxWaiting lists the inbox's files whose document is not stored yet:
// waiting to be picked up, queued, refused for their size, or failed
// before the document was stored. It lists only what the watch would
// take: dot files, symbolic links and names the pattern does not match
// are skipped, as the watch skips them (ADR 0017, item 16).
func (s *Server) inboxWaiting(ctx context.Context, reader store.Reader, stored map[string]bool) ([]DocumentView, error) {
	in := s.inboxConfig()
	if in.Dir == "" {
		return nil, nil
	}
	pattern := in.Pattern
	if pattern == "" {
		pattern = "*"
	}
	entries, err := os.ReadDir(in.Dir)
	if err != nil {
		s.opts.Logger.Warn("ui: cannot list the inbox", "error", err.Error())
		return nil, nil
	}
	out := make([]DocumentView, 0, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			continue
		}
		if ok, _ := filepath.Match(pattern, e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		v := DocumentView{Name: e.Name(), Received: fi.ModTime(), Records: []RecordView{}}
		if in.MaxBytes > 0 && fi.Size() > in.MaxBytes {
			v.State, v.Label, v.Kind = stateFailed, "Too large to read", kindFailed
			v.Detail = fmt.Sprintf("over the %d MiB limit", in.MaxBytes>>20)
			out = append(out, v)
			continue
		}
		sha, err := s.inbox.hash(filepath.Join(in.Dir, e.Name()), fi)
		if err != nil {
			continue // gone, or being written: the next listing sees it
		}
		if stored[sha] {
			continue
		}
		if err := unstoredState(ctx, reader, sha, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// unstoredState sets the state of an inbox file whose document is not
// stored, from its run: none yet, runnable, or ended without the document.
func unstoredState(ctx context.Context, reader store.Reader, sha string, v *DocumentView) error {
	run, err := reader.Run(ctx, review.DocumentInstance(sha))
	switch {
	case errors.Is(err, store.ErrNotFound):
		v.State, v.Label, v.Kind = stateQueued, "Waiting to be picked up", kindWorking
	case err != nil:
		return err //nolint:wrapcheck // the store names it
	case run.Status == store.StatusFailed:
		v.State, v.Label, v.Kind, v.Detail = stateFailed, "The agent could not read it", kindFailed, failureDetail(&run)
	case run.Status.Terminal():
		v.State, v.Label, v.Kind = stateFailed, "Stopped", kindFailed
	default:
		v.State, v.Label, v.Kind = stateQueued, "Queued", kindWorking
	}
	return nil
}
