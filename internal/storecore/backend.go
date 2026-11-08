// Package storecore is the engine behind the reference stores (memstore and
// filestore): every semantic of the store interface (fencing, claims,
// preconditions, object versions, run creation, queued triggers) over a
// Backend that only reads, lists and atomically applies files. The two
// stores differ only in their Backend, so they cannot drift apart in
// meaning; the file store adds the lock, the journal and recovery.
package storecore

// Write is one file a commit writes or deletes. Paths use '/' separators
// and are relative to the store's root.
type Write struct {
	Path   string
	Data   []byte
	Delete bool
}

// Backend holds the store's files.
//
// The engine serializes every call: Apply is never concurrent with any
// other call, and Read and List run only alongside other reads.
type Backend interface {
	// Read returns a file's contents, with ok false if it does not exist.
	Read(path string) (data []byte, ok bool, err error)

	// List returns the names of a directory's entries (files and
	// directories), excluding temporary files. A missing directory has
	// none.
	List(dir string) ([]string, error)

	// Apply applies every write or none. An error from before the commit
	// point means nothing was applied. An error from after it must be an
	// *IndeterminateError: the engine then refuses every later call until
	// the store is reopened (ADR 0005, item 14).
	Apply(writes []Write) error

	// Close releases the backend.
	Close() error
}

// IndeterminateError reports a failure after Apply's commit point: the
// writes may or may not have landed.
type IndeterminateError struct {
	Err error
}

func (e *IndeterminateError) Error() string {
	return "store: failure after the commit point, outcome indeterminate: " + e.Err.Error()
}

func (e *IndeterminateError) Unwrap() error { return e.Err }
