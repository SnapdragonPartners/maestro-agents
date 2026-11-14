package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// FileTrigger is the payload of a one-shot or folder trigger (ADR 0017,
// item 11): the input file the run processes. An agent type reads it from
// agent.Env.Trigger with ParseFileTrigger, and the file with Read.
type FileTrigger struct {
	// Path is the file's absolute path.
	Path string `json:"path"`
	// Name is the file's base name.
	Name string `json:"name"`
	// SHA256 is the hex SHA-256 of the file's bytes when the host read
	// them: the run's identity.
	SHA256 string `json:"sha256"`
	// Size is the file's size in bytes when the host read it.
	Size int64 `json:"size"`
}

// ErrInputChanged is returned by FileTrigger.Read for a file whose bytes
// are no longer those the trigger names.
var ErrInputChanged = errors.New("standalone: input file changed since its trigger")

// ParseFileTrigger decodes a trigger payload.
func ParseFileTrigger(raw json.RawMessage) (FileTrigger, error) {
	var t FileTrigger
	if err := json.Unmarshal(raw, &t); err != nil {
		return FileTrigger{}, fmt.Errorf("standalone: file trigger: %w", err)
	}
	if t.Path == "" || len(t.SHA256) != sha256.Size*2 || t.Size < 0 {
		return FileTrigger{}, errors.New("standalone: file trigger: path, sha256 and size are required")
	}
	return t, nil
}

// Read returns the file's bytes, refusing with ErrInputChanged a file
// whose size or SHA-256 differs from the trigger's: the host leaves input
// files in place, so a file can change after its trigger, and a run
// processes the bytes its identity names.
func (t FileTrigger) Read() ([]byte, error) { //nolint:gocritic // hugeParam: a small value, read once per run
	f, err := os.Open(t.Path)
	if err != nil {
		return nil, fmt.Errorf("standalone: read input: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, t.Size+1))
	if err != nil {
		return nil, fmt.Errorf("standalone: read input: %w", err)
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != t.Size || hex.EncodeToString(sum[:]) != t.SHA256 {
		return nil, ErrInputChanged
	}
	return data, nil
}

// ID prefixes (ADR 0017, item 11).
const (
	triggerIDPrefix  = "sha256:"
	instanceIDPrefix = "sha256-"
)

// input is a hashed input file and the identities it gives its run.
type input struct {
	id       string // trigger ID
	instance string // instance and run ID
	trigger  FileTrigger
	payload  json.RawMessage
}

// errTooLarge is hashInput's refusal of a file over the limit.
var errTooLarge = errors.New("larger than max_input_bytes")

// hashInput reads and hashes an input file of at most limit bytes. It
// refuses anything but a regular file.
func hashInput(path string, limit int64) (*input, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller names the input
	}
	if !utf8.ValidString(abs) {
		// JSON would replace the invalid bytes, and the payload would name
		// another file.
		return nil, errors.New("its path is not valid UTF-8")
	}
	f, err := os.Open(abs) //nolint:gosec // G304: the operator's input
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err //nolint:wrapcheck // the PathError names the file
	}
	if n > limit {
		return nil, errTooLarge
	}
	sum := hex.EncodeToString(h.Sum(nil))
	t := FileTrigger{Path: abs, Name: filepath.Base(abs), SHA256: sum, Size: n}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(t); err != nil {
		return nil, err //nolint:wrapcheck // strings and an integer always encode
	}
	return &input{trigger: t, payload: bytes.TrimSpace(buf.Bytes()), id: triggerIDPrefix + sum, instance: instanceIDPrefix + sum}, nil
}
