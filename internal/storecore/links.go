package storecore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/store"
)

// ErrMigrationRequired refuses to serve a store that holds artifacts stored
// before artifact-to-instance links existed (ADR 0024, item 9), until a
// migration has linked each of them to the instances that own it.
var ErrMigrationRequired = errors.New("store: artifact links migration required")

// UnclaimedArtifactsError refuses a link migration because some stored
// artifacts are named by no instance's records and by no explicit owner.
// Nothing was written. It wraps [ErrMigrationRequired].
type UnclaimedArtifactsError struct {
	// Digests are the unclaimed artifacts, sorted.
	Digests []string
}

func (e *UnclaimedArtifactsError) Error() string {
	return fmt.Sprintf("store: artifact links migration: %d stored artifacts are referenced by no instance and have no stated owner: %s",
		len(e.Digests), strings.Join(e.Digests, ", "))
}

func (e *UnclaimedArtifactsError) Unwrap() error { return ErrMigrationRequired }

// LinkMigration says how to link a store's legacy artifacts.
type LinkMigration struct {
	// References returns the artifact digests an instance's own records
	// name, read through the store. Nil refuses a store with legacy
	// artifacts with ErrMigrationRequired, whatever Owners states.
	References func(ctx context.Context, r store.Reader, instance string) ([]string, error)
	// Owners states owners explicitly, in addition to References: each
	// digest, which must be stored, is linked to each instance listed,
	// which must exist.
	Owners map[string][]string
}

type linksMarker struct {
	Version int `json:"version"`
}

// MigrateArtifactLinks brings a store written before artifact links
// existed up to date, once: it links each stored artifact to every
// instance whose records name it (m.References) and to every owner m
// states, then writes the marker, all in one atomic apply. A store already
// marked returns at once; one holding no artifacts is only marked. An
// artifact nobody claims fails the migration with an
// [*UnclaimedArtifactsError] and nothing written.
//
// It must run before the store is shared: it reads through the engine's
// own reader, without holding the engine's lock across those reads.
func (e *Engine) MigrateArtifactLinks(ctx context.Context, m LinkMigration) error {
	marked, stored, instances, err := e.legacy(ctx)
	if err != nil || marked {
		return err
	}
	links := map[string]map[string]bool{} // digest -> encoded instances
	if len(stored) > 0 {
		if m.References == nil {
			return fmt.Errorf("%w: the store holds %d artifacts stored before artifact links existed; open it with a references function (filestore.WithLegacyArtifactLinks(agent.ArtifactReferences)), which stated owners only add to", ErrMigrationRequired, len(stored))
		}
		if links, err = claim(ctx, e, m, stored, instances); err != nil {
			return err
		}
	}
	marker, err := marshal(linksMarker{Version: 1})
	if err != nil {
		return err
	}
	link, err := marshal(linkFile{})
	if err != nil {
		return err
	}
	writes := []Write{}
	for _, d := range sortedKeys(links) {
		for _, inst := range sortedKeys(links[d]) {
			writes = append(writes, Write{Path: linkPath(inst, d), Data: link})
		}
	}
	writes = append(writes, Write{Path: linksMarkerPath, Data: marker})
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return err
	}
	return e.apply(writes)
}

// legacy reports whether the store is marked as holding links and, if
// not, its stored artifacts and, if it has any, its instances.
func (e *Engine) legacy(ctx context.Context) (marked bool, stored map[string]bool, instances map[string]string, err error) {
	if err = e.rlock(ctx); err != nil {
		return false, nil, nil, err
	}
	defer e.mu.RUnlock()
	_, marked, err = e.b.Read(linksMarkerPath)
	if err == nil && !marked {
		stored, err = e.storedArtifacts()
	}
	if err == nil && !marked && len(stored) > 0 {
		instances, err = e.instances()
	}
	if err != nil {
		return false, nil, nil, fmt.Errorf("store: artifact links migration: %w", err)
	}
	return marked, stored, instances, nil
}

// claim maps each stored digest to the encoded instances that own it, and
// refuses an artifact nobody owns.
func claim(ctx context.Context, r store.Reader, m LinkMigration, stored map[string]bool, instances map[string]string) (map[string]map[string]bool, error) {
	links := map[string]map[string]bool{}
	add := func(d, inst string) {
		if links[d] == nil {
			links[d] = map[string]bool{}
		}
		links[d][inst] = true
	}
	for _, instance := range sortedKeys(instances) {
		refs, err := m.References(ctx, r, instance)
		if err != nil {
			return nil, fmt.Errorf("store: artifact links migration: references of instance %q: %w", instance, err)
		}
		for _, d := range refs {
			// A reference to content the store does not hold links
			// nothing: no link may point at missing content.
			if stored[d] {
				add(d, instances[instance])
			}
		}
	}
	for _, d := range sortedKeys(m.Owners) {
		if !stored[d] {
			return nil, fmt.Errorf("%w: artifact links migration: stated owner of artifact %q, which is not stored", store.ErrInvalid, d)
		}
		for _, instance := range m.Owners[d] {
			inst, ok := instances[instance]
			if !ok {
				return nil, fmt.Errorf("%w: artifact links migration: artifact %s stated as owned by instance %q, which does not exist", store.ErrInvalid, d, instance)
			}
			add(d, inst)
		}
	}
	var unclaimed []string
	for d := range stored {
		if links[d] == nil {
			unclaimed = append(unclaimed, d)
		}
	}
	if len(unclaimed) > 0 {
		slices.Sort(unclaimed)
		return nil, &UnclaimedArtifactsError{Digests: unclaimed}
	}
	return links, nil
}

// storedArtifacts returns the digests whose metadata the store holds. The
// caller holds mu.
func (e *Engine) storedArtifacts() (map[string]bool, error) {
	names, err := e.b.List("artifacts")
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	out := map[string]bool{}
	for _, name := range names {
		if d, ok := strings.CutSuffix(name, ".json"); ok && validDigest(d) {
			out[d] = true
		}
	}
	return out, nil
}

// instances returns the store's instances, each mapped to its encoded
// path component. The caller holds mu.
func (e *Engine) instances() (map[string]string, error) {
	names, err := e.b.List("instances")
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := map[string]string{}
	for _, name := range names {
		if instance, ok := decode(name); ok {
			out[instance] = name
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
