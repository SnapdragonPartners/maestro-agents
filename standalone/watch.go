package standalone

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// The retry delay after an activation without a confirmed outcome (ADR
// 0017, item 17).
const (
	minRetryDelay = time.Second
	maxRetryDelay = 5 * time.Minute
)

// fileState is what a scan saw of a file.
type fileState struct {
	modTime time.Time
	size    int64
	// stable: seen unchanged by two consecutive scans.
	stable bool
	// done: its run exists, or it was refused (too large, unreadable as
	// a regular file); it is not hashed again until it changes.
	done bool
}

// scanner finds the watched folder's new files (ADR 0017, item 16).
type scanner struct {
	files   map[string]*fileState
	dir     string
	pattern string
}

// scan returns the names of the files that have become stable since the
// last scan and have no run yet.
func (s *scanner) scan() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the folder
	}
	present := map[string]bool{}
	ready := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !e.Type().IsRegular() {
			continue // dotfiles, directories, symbolic links and devices
		}
		if ok, _ := filepath.Match(s.pattern, name); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // removed since the listing
		}
		present[name] = true
		prev := s.files[name]
		if prev == nil || prev.size != fi.Size() || !prev.modTime.Equal(fi.ModTime()) {
			s.files[name] = &fileState{size: fi.Size(), modTime: fi.ModTime()}
			continue
		}
		if prev.done {
			continue
		}
		prev.stable = true
		ready = append(ready, name)
	}
	for name := range s.files {
		if !present[name] {
			delete(s.files, name)
		}
	}
	return ready, nil
}

// watcher is a folder watch's scheduler state.
type watcher struct {
	*host
	scan *scanner
	// inflight holds the instances this process is activating.
	inflight map[string]bool
	// notBefore holds each instance's retry time, and backoff its delay.
	notBefore map[string]time.Time
	backoff   map[string]time.Duration
	// refused holds runs not to activate again while the process lives:
	// ErrNotActivatable, or another configuration's.
	refused map[string]bool
	results chan *result
	// interrupted counts activations a shutdown or a store failure left
	// unfinished.
	interrupted int
	fatal       bool
}

// watch runs the folder watch until ctx ends or the store must be
// reopened, and returns the exit code (ADR 0017, items 16, 21 and 22).
func (h *host) watch(ctx context.Context) int {
	w := &watcher{
		host:      h,
		scan:      &scanner{dir: h.cfg.Trigger.Dir, pattern: h.cfg.Trigger.pattern(), files: map[string]*fileState{}},
		inflight:  map[string]bool{},
		notBefore: map[string]time.Time{},
		backoff:   map[string]time.Duration{},
		refused:   map[string]bool{},
		results:   make(chan *result),
	}
	wctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	tick := time.NewTicker(h.cfg.Trigger.pollInterval())
	defer tick.Stop()
	h.logger.Info("standalone: watching", "agent_type", h.cfg.AgentType, "concurrency", h.concurrency())
	for {
		if !w.fatal && wctx.Err() == nil {
			w.dispatch(wctx) // claimable runs first: unfinished work resumes at once
			w.scanOnce(wctx)
			w.dispatch(wctx)
		}
		if w.fatal || wctx.Err() != nil {
			stop(nil)
			for len(w.inflight) > 0 {
				w.done(<-w.results, true)
			}
			return w.exitCode()
		}
		select {
		case <-wctx.Done():
		case r := <-w.results:
			w.done(r, wctx.Err() != nil)
		case <-tick.C:
		}
	}
}

func (h *host) concurrency() int { return max(h.cfg.Concurrency, 1) }

func (w *watcher) exitCode() int {
	if w.fatal || w.interrupted > 0 {
		return ExitIncomplete
	}
	return ExitComplete
}

// scanOnce makes a run for each new stable file.
func (w *watcher) scanOnce(ctx context.Context) {
	names, err := w.scan.scan()
	if err != nil {
		w.logger.Warn("standalone: cannot scan the watched folder", "error_class", scanErrorClass(err))
		return
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		st := w.scan.files[name]
		in, err := hashInput(filepath.Join(w.scan.dir, name), w.maxInput, true)
		if err != nil {
			if errors.Is(err, errTooLarge) || errors.Is(err, fs.ErrNotExist) {
				st.done = true
			}
			w.logger.Warn("standalone: input refused", "error_class", inputErrorClass(err))
			continue
		}
		if _, err := w.makeRun(context.WithoutCancel(ctx), in, agent.TriggerFolder); err != nil {
			w.logger.Error("standalone: cannot make the input's run", "instance", in.instance, "error_class", errorClass(err))
			switch {
			case errors.Is(err, store.ErrUnusable):
				w.fatal = true
				return
			case errors.Is(err, errAnotherConfiguration):
				st.done = true // not tried again until it changes
			}
			continue // tried again at the next scan
		}
		st.done = true
	}
}

// dispatch activates claimable runs up to the concurrency cap.
func (w *watcher) dispatch(ctx context.Context) {
	if len(w.inflight) >= w.concurrency() {
		return
	}
	claimable, err := w.st.Claimable(ctx)
	if err != nil {
		w.logger.Error("standalone: cannot list claimable runs", "error_class", errorClass(err))
		w.fatal = w.fatal || errors.Is(err, store.ErrUnusable)
		return
	}
	now := time.Now()
	for _, c := range claimable {
		if len(w.inflight) >= w.concurrency() || ctx.Err() != nil {
			return
		}
		if w.inflight[c.Instance] || w.refused[c.Run] || now.Before(w.notBefore[c.Instance]) {
			continue
		}
		run, err := w.st.Run(ctx, c.Run)
		if err != nil {
			w.logger.Error("standalone: cannot read a claimable run", "run", c.Run, "error_class", errorClass(err))
			if errors.Is(err, store.ErrUnusable) {
				w.fatal = true
				return
			}
			continue
		}
		if run.AgentType != w.cfg.AgentType {
			w.refused[c.Run] = true // another configuration's (ADR 0017, item 9)
			continue
		}
		w.inflight[c.Instance] = true
		go func() { w.results <- w.activate(ctx, &run) }()
	}
}

// done records an activation's result. During a shutdown, a run left
// unfinished counts against the exit code; otherwise it is retried after a
// delay (ADR 0017, items 17 and 22).
func (w *watcher) done(r *result, shuttingDown bool) {
	instance := r.Instance
	delete(w.inflight, instance)
	if r.Fatal {
		w.fatal = true
	}
	switch {
	case !r.interrupted():
		delete(w.backoff, instance)
		delete(w.notBefore, instance)
	case shuttingDown:
		w.interrupted++
	case errors.Is(r.Err, agent.ErrNotActivatable):
		w.refused[r.Run] = true
	default:
		d := min(max(w.backoff[instance]*2, minRetryDelay), maxRetryDelay)
		w.backoff[instance] = d
		w.notBefore[instance] = time.Now().Add(d)
	}
}

func scanErrorClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	}
	return "other"
}

func inputErrorClass(err error) string {
	if errors.Is(err, errTooLarge) {
		return "too_large"
	}
	return scanErrorClass(err)
}
