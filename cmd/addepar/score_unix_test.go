//go:build unix

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
)

// TestScoreRefusesAFIFO: a -key or -map that is a FIFO is refused as not a
// regular file, not hung on (ADR 0017, item 11).
func TestScoreRefusesAFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	key := filepath.Join(dir, "key.csv") // the header alone: a valid, empty key
	if err := os.WriteFile(key, []byte(strings.Join(export.Columns(), ",")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"answer key": {"score", "-config", "c.json", "-inputs", dir, "-key", fifo, "-map", fifo},
		"mapping":    {"score", "-config", "c.json", "-inputs", dir, "-key", key, "-map", fifo},
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			var code int
			var errb bytes.Buffer
			go func() {
				defer close(done)
				code = run(context.Background(), program(nil), args, &bytes.Buffer{}, &errb)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("score hung on a FIFO")
			}
			if code != exitUsage || !strings.Contains(errb.String(), name+": read:") || !strings.Contains(errb.String(), "not a regular file") {
				t.Fatalf("exit %d: %s", code, errb.String())
			}
		})
	}
}
