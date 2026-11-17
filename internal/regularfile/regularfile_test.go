package regularfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCapped(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadCapped(f, 4); err != nil || string(b) != "abcd" {
		t.Fatalf("at the limit: %q, %v", b, err)
	}
	if _, err := ReadCapped(f, 3); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := ReadCapped(dir, 10); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a directory: %v", err)
	}
	if _, err := ReadCapped(filepath.Join(dir, "none"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}
