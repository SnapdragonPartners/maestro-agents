package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// ArtifactReferences returns the digests of the artifacts an instance's own
// records name, for a store migrating to artifact-to-instance links (ADR
// 0024, item 9; filestore.WithLegacyArtifactLinks). Only the runtime writes
// artifacts, and it names each one in a typed field of its own records: a
// run_started entry's manifest, configuration and served definitions, a
// model_attempt entry's parameters and messages, a turn entry's response
// and message, a result entry's result, and a checkpoint's configuration,
// served definitions, model parameters and transcript. It reads those
// fields alone, never an agent type's state data or an entry kind it does
// not recognize, so a digest that merely appears in an agent's data is not
// a reference. A recognized entry whose payload does not decode, or a
// checkpoint in a format this release does not know, is an error. The
// result is sorted and without repeats.
func ArtifactReferences(ctx context.Context, r store.Reader, instance string) ([]string, error) {
	refs := map[string]bool{}
	add := func(ds ...string) {
		for _, d := range ds {
			if d != "" {
				refs[d] = true
			}
		}
	}
	q := store.RunQuery{}
	for {
		runs, more, err := r.Runs(ctx, instance, q)
		if err != nil {
			return nil, fmt.Errorf("agent: artifact references of %q: runs: %w", instance, err)
		}
		for i := range runs {
			if err := entryReferences(ctx, r, runs[i].ID, add); err != nil {
				return nil, err
			}
		}
		if !more || len(runs) == 0 {
			break
		}
		q.After = runs[len(runs)-1].ID
	}
	latest, ok, err := r.LatestCheckpoint(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("agent: artifact references of %q: latest checkpoint: %w", instance, err)
	}
	if ok {
		for seq := uint64(1); seq <= latest.Seq; seq++ {
			c, err := r.Checkpoint(ctx, instance, seq)
			if err != nil {
				return nil, fmt.Errorf("agent: artifact references of %q: checkpoint %d: %w", instance, seq, err)
			}
			if err := checkpointReferences(&c, add); err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(refs))
	for d := range refs {
		out = append(out, d)
	}
	slices.Sort(out)
	return out, nil
}

// entryReferences adds the digests a run's entries name.
func entryReferences(ctx context.Context, r store.Reader, run string, add func(...string)) error {
	entries, err := r.Entries(ctx, run, 0)
	if err != nil {
		return fmt.Errorf("agent: artifact references of run %q: entries: %w", run, err)
	}
	for i := range entries {
		e := entries[i]
		var ds []string
		switch e.Kind {
		case EntryRunStarted:
			var p RunStarted
			err = decodeEntry(&e, &p)
			ds = []string{p.Manifest, p.Config, p.Served}
		case EntryModelAttempt:
			var p ModelAttemptEntry
			err = decodeEntry(&e, &p)
			ds = append([]string{p.Params, p.StateParams}, p.Messages...)
		case EntryTurn:
			var p TurnEntry
			err = decodeEntry(&e, &p)
			ds = []string{p.Response, p.Message}
		case EntryResult:
			var p ResultEntry
			err = decodeEntry(&e, &p)
			ds = []string{p.Result}
		default:
			continue
		}
		if err != nil {
			return err
		}
		add(ds...)
	}
	return nil
}

func decodeEntry(e *store.Entry, v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("agent: artifact references: run %q entry %d (%s): %w", e.Run, e.Seq, e.Kind, err)
	}
	return nil
}

// checkpointReferences adds the digests a checkpoint's envelope names,
// never its agent type's data.
func checkpointReferences(c *store.Checkpoint, add func(...string)) error {
	if c.Format != checkpointFormat {
		return fmt.Errorf("agent: artifact references: instance %q checkpoint %d has format %d, want %d", c.Instance, c.Seq, c.Format, checkpointFormat)
	}
	var cs checkpointState
	if err := json.Unmarshal(c.State, &cs); err != nil {
		return fmt.Errorf("agent: artifact references: instance %q checkpoint %d: %w", c.Instance, c.Seq, err)
	}
	add(cs.Config, cs.Served)
	if m := cs.Model; m != nil {
		add(m.Params)
		add(m.Transcript...)
	}
	return nil
}
