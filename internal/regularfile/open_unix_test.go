//go:build unix

package regularfile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOpenRefusesFIFOAndLink(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := Open(fifo, false); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a FIFO: %v", err)
	}
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, true); err == nil {
		t.Fatal("noFollow opened a link")
	}
	g, err := Open(link, false)
	if err != nil {
		t.Fatalf("a followed link: %v", err)
	}
	_ = g.Close()
}
