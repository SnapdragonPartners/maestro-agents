//go:build unix

package standalone

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// TestFIFOInputsRefused: an input, a secrets file or a configuration that
// is a FIFO is refused without blocking (Codex round 1 on 651edbc; ADR
// 0017, item 11). Unix only: elsewhere there are no FIFOs to open.
func TestFIFOInputsRefused(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 3)
	go func() {
		_, err := hashInput(fifo, 1024, false)
		done <- err
		_, err = readSecretsFile(fifo)
		done <- err
		_, err = LoadConfig(fifo)
		done <- err
	}()
	for range 3 {
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("FIFO: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("blocked opening a FIFO")
		}
	}
}

// TestOpenSinkFIFO: a jsonl sink path that is a FIFO with no reader is
// refused at once, not hung on (Codex round 2 on 04e40e4; ADR 0017, item
// 20).
func TestOpenSinkFIFO(t *testing.T) {
	st := memstore.New()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := openSink(&TelemetryConfig{Sink: SinkJSONL, Path: fifo}, st)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("opened a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked opening a FIFO")
	}
}
