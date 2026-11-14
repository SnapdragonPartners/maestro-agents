package standalone

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

func TestCredentials(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets.json")
	writeFile(t, secrets, `{"db":"file-value"}`)
	env := map[string]string{"API_KEY": "env-value"}
	getenv := func(k string) string { return env[k] }
	cfg := &CredentialsConfig{Env: map[string]string{"api": "API_KEY"}, File: secrets}
	creds, err := loadCredentials(cfg, "alice", getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, want := range map[string]string{"api": "env-value", "db": "file-value"} {
		s, err := creds.provider(ctx, &agent.RunContext{Principal: "alice"}, name)
		if err != nil || s.Reveal() != want {
			t.Errorf("%s: %q, %v", name, s.Reveal(), err)
		}
	}
	if _, err := creds.provider(ctx, &agent.RunContext{Principal: "alice"}, "other"); !errors.Is(err, agent.ErrNoCredential) {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := creds.provider(ctx, &agent.RunContext{Principal: "mallory"}, "api"); !errors.Is(err, agent.ErrNoCredential) {
		t.Errorf("another principal: %v", err)
	}
	if _, err := creds.provider(ctx, nil, "api"); !errors.Is(err, agent.ErrNoCredential) {
		t.Errorf("no run context: %v", err)
	}
	if s, ok := creds.get("api"); !ok || s.Reveal() != "env-value" {
		t.Errorf("get: %v", ok)
	}
}

func TestCredentialsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	none := func(string) string { return "" }
	cases := map[string]struct {
		body string
		mode os.FileMode
		cfg  CredentialsConfig
		env  func(string) string
		want string
	}{
		"group readable":  {`{"a":"1"}`, 0o640, CredentialsConfig{File: path}, none, "accessible by its group or others"},
		"world readable":  {`{"a":"1"}`, 0o604, CredentialsConfig{File: path}, none, "accessible by its group or others"},
		"in both":         {`{"a":"1"}`, 0o600, CredentialsConfig{File: path, Env: map[string]string{"a": "A"}}, func(string) string { return "x" }, "in both"},
		"empty value":     {`{"a":""}`, 0o600, CredentialsConfig{File: path}, none, "is empty"},
		"repeated key":    {`{"a":"1","a":"2"}`, 0o600, CredentialsConfig{File: path}, none, "not a JSON object"},
		"not strings":     {`{"a":1}`, 0o600, CredentialsConfig{File: path}, none, "not a JSON object"},
		"not an object":   {`["a"]`, 0o600, CredentialsConfig{File: path}, none, "not a JSON object"},
		"null":            {`null`, 0o600, CredentialsConfig{File: path}, none, "not a JSON object"},
		"empty":           {`{}`, 0o600, CredentialsConfig{File: path}, none, "holds no credentials"},
		"unset env":       {`{"a":"1"}`, 0o600, CredentialsConfig{Env: map[string]string{"b": "B"}}, none, "B is unset or empty"},
		"missing file":    {``, 0, CredentialsConfig{File: filepath.Join(dir, "absent")}, none, "no such file"},
		"secret not echo": {`{"a":"s3cr3t`, 0o600, CredentialsConfig{File: path}, none, "not a JSON object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.mode != 0 {
				writeFile(t, path, tc.body)
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			_, err := loadCredentials(&tc.cfg, "", tc.env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("the error quotes a secret: %v", err)
			}
		})
	}
	if _, err := loadCredentials(&CredentialsConfig{File: dir}, "", none); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
}

func TestHashInputAndRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "statement.pdf")
	writeFile(t, path, "hello")
	in, err := hashInput(path, 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	const sum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if in.id != "sha256:"+sum || in.instance != "sha256-"+sum || in.trigger.Name != "statement.pdf" || in.trigger.Size != 5 || !filepath.IsAbs(in.trigger.Path) {
		t.Fatalf("input %+v", in)
	}
	ft, err := ParseFileTrigger(in.payload)
	if err != nil || ft != in.trigger {
		t.Fatalf("payload round trip: %+v, %v", ft, err)
	}
	if b, err := ft.Read(); err != nil || string(b) != "hello" {
		t.Fatalf("Read: %q, %v", b, err)
	}
	writeFile(t, path, "jello")
	if _, err := ft.Read(); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("same size, other bytes: %v", err)
	}
	writeFile(t, path, "hello!")
	if _, err := ft.Read(); !errors.Is(err, ErrInputChanged) {
		t.Fatalf("grown: %v", err)
	}
	if _, err := hashInput(path, 5, false); !errors.Is(err, errTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := hashInput(dir, 1024, false); err == nil {
		t.Fatal("hashed a directory")
	}
	if _, err := hashInput(filepath.Join(dir, "absent"), 1024, false); err == nil {
		t.Fatal("hashed a missing file")
	}
	for _, bad := range []string{`{}`, `{"path":"/x","sha256":"ab","size":1}`, `nope`} {
		if _, err := ParseFileTrigger(json.RawMessage(bad)); err == nil {
			t.Errorf("parsed %s", bad)
		}
	}
}

// TestInputAdmission covers the findings of Codex round 1 on 651edbc: a
// symbolic link in a watched folder is refused rather than followed, a
// FIFO is refused without blocking, and the largest max_input_bytes does
// not overflow (ADR 0017, items 11 and 16).
func TestInputAdmission(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "elsewhere")
	link := filepath.Join(dir, "ready.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := hashInput(link, 1024, true); err == nil {
		t.Fatal("followed a symbolic link out of a watched folder")
	}
	if in, err := hashInput(link, 1024, false); err != nil || in.trigger.Size != 9 {
		t.Fatalf("an operator's -input link: %+v, %v", in, err)
	}

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

	path := filepath.Join(dir, "in.txt")
	writeFile(t, path, "hello")
	in, err := hashInput(path, math.MaxInt64, false)
	if err != nil || in.trigger.Size != 5 || in.trigger.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("max limit: %+v, %v", in, err)
	}
	if b, err := readLimited(strings.NewReader("abc"), math.MaxInt64); err != nil || string(b) != "abc" {
		t.Fatalf("readLimited at the max: %q, %v", b, err)
	}
	if b, err := (FileTrigger{Path: path, SHA256: in.trigger.SHA256, Size: 5}).Read(); err != nil || string(b) != "hello" {
		t.Fatalf("Read: %q, %v", b, err)
	}
}

func TestJSONLSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s, err := openJSONL(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evs := []agent.Event{{Run: fmt.Sprintf("r%d", i), Seq: 1, Kind: "a"}, {Run: fmt.Sprintf("r%d", i), Seq: 2, Kind: "b"}}
			if eerr := s.Emit(context.Background(), evs); eerr != nil {
				t.Error(eerr)
			}
		}()
	}
	wg.Wait()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v, %v", fi.Mode(), err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	last := map[string]uint64{}
	n := 0
	for sc.Scan() {
		var ev agent.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		if ev.Seq != last[ev.Run]+1 {
			t.Fatalf("run %s: seq %d after %d: batches interleaved", ev.Run, ev.Seq, last[ev.Run])
		}
		last[ev.Run] = ev.Seq
		n++
	}
	if n != 16 {
		t.Fatalf("%d lines", n)
	}
}

func TestOpenSink(t *testing.T) {
	st := memstore.New()
	if s, c, err := openSink(&TelemetryConfig{}, st); s != nil || c != nil || err != nil {
		t.Fatalf("none: %v %v %v", s, c, err)
	}
	if s, _, err := openSink(&TelemetryConfig{Sink: SinkStore}, st); s == nil || err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, _, err := openSink(&TelemetryConfig{Sink: SinkStore}, noTelemetry{st}); err == nil {
		t.Fatal("a store without telemetry storage")
	}
	if _, _, err := openSink(&TelemetryConfig{Sink: SinkJSONL, Path: filepath.Join(t.TempDir(), "no", "dir")}, st); err == nil {
		t.Fatal("an unopenable path")
	}
	// A FIFO with no reader is refused at once, not hung on (Codex round
	// 2 on 04e40e4), and so is a directory.
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
	if _, _, err := openSink(&TelemetryConfig{Sink: SinkJSONL, Path: dir}, st); err == nil {
		t.Fatal("opened a directory")
	}
}

// noTelemetry hides a store's store.Telemetry.
type noTelemetry struct{ store.Store }

func TestScanner(t *testing.T) {
	dir := t.TempDir()
	s := &scanner{dir: dir, pattern: "*.pdf", files: map[string]*fileState{}}
	scan := func() []string {
		t.Helper()
		names, err := s.scan()
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(names)
		return names
	}
	writeFile(t, filepath.Join(dir, "a.pdf"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, ".c.pdf"), "c")
	if err := os.Mkdir(filepath.Join(dir, "d.pdf"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.pdf")
	writeFile(t, outside, "x")
	if err := os.Symlink(outside, filepath.Join(dir, "e.pdf")); err != nil {
		t.Fatal(err)
	}
	if got := scan(); len(got) != 0 {
		t.Fatalf("first sight is not stable: %v", got)
	}
	if got := scan(); !slices.Equal(got, []string{"a.pdf"}) {
		t.Fatalf("second scan: %v (only regular, matching, visible files)", got)
	}
	if got := scan(); !slices.Equal(got, []string{"a.pdf"}) {
		t.Fatalf("not done yet, so offered again: %v", got)
	}
	s.files["a.pdf"].done = true
	if got := scan(); len(got) != 0 {
		t.Fatalf("done: %v", got)
	}
	// A changed file is new: it waits to be stable again.
	writeFile(t, filepath.Join(dir, "a.pdf"), "a2")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.pdf"), future, future); err != nil {
		t.Fatal(err)
	}
	if got := scan(); len(got) != 0 {
		t.Fatalf("changed: %v", got)
	}
	if got := scan(); !slices.Equal(got, []string{"a.pdf"}) {
		t.Fatalf("changed, then stable: %v", got)
	}
	// A removed file is forgotten.
	if err := os.Remove(filepath.Join(dir, "a.pdf")); err != nil {
		t.Fatal(err)
	}
	scan()
	if _, ok := s.files["a.pdf"]; ok {
		t.Fatal("a removed file is remembered")
	}
	s.dir = filepath.Join(dir, "absent")
	if _, err := s.scan(); err == nil {
		t.Fatal("scanned a missing folder")
	}
}

func TestErrorClass(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("x: %w", agent.ErrOwnershipLost):                    "ownership_lost",
		fmt.Errorf("%w: %w", agent.ErrStoreFailure, store.ErrUnusable): "store_unusable",
		fmt.Errorf("%w: boom", agent.ErrStoreFailure):                  "store_failure",
		errors.New("anything"):                                         "other",
	} {
		if got := errorClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
