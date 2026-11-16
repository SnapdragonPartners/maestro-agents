package intake_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/filestore"
)

// The slice's steps 3 to 5 (requirements section 6) on the intake agent
// itself, against the file store: an activation stopped at every durable
// boundary and continued in a new process (TEST-5), and both stale-owner
// interleavings against the submission's commit (TEST-6).

// sliceScript reads the statement, submits the total-fund column (refused
// by check 4) and then the right record.
func sliceScript(t testing.TB) []llms.ChatResponse {
	t.Helper()
	c := synthetic(t, "keelson-vi")
	return []llms.ChatResponse{
		getDocument("1"),
		submit("2", mutated(t, c, "total-fund column chosen")),
		submit("3", c.Record),
	}
}

func openFile(t testing.TB, dir string, clk *clock, opts ...filestore.Option) store.Store {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	s, err := filestore.Open(dir, append([]filestore.Option{filestore.WithClock(clk.Now)}, opts...)...)
	if err != nil {
		t.Fatalf("filestore.Open: %v", err)
	}
	return s
}

// crashing fails one commit, the at'th from 1, as a process dying there
// would: before it lands, or after it landed and before its result returns.
type crashing struct {
	store.Store
	n, at   int
	landing bool
}

var errCrash = errors.New("injected crash")

func (c *crashing) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.n++
	if c.n != c.at {
		return c.Store.Commit(ctx, f, ops) //nolint:wrapcheck // a transparent wrapper
	}
	if c.landing {
		if err := c.Store.Commit(ctx, f, ops); err != nil {
			return err //nolint:wrapcheck // a transparent wrapper
		}
	}
	return errCrash
}

type counting struct {
	store.Store
	n int
}

func (c *counting) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	c.n++
	return c.Store.Commit(ctx, f, ops) //nolint:wrapcheck // a transparent wrapper
}

// checkSlice checks the finished run: succeeded, the document stored once,
// exactly the two attempts (the refused one and the verified one), one
// record, the verified outcome, and every model turn recorded once, so no
// turn was asked of the model again once recorded and no work was lost.
func checkSlice(t testing.TB, s store.Store, sha string) {
	t.Helper()
	ctx := context.Background()
	r, err := s.Run(ctx, runID(sha))
	if err != nil || r.Status != store.StatusSucceeded {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if keys := listKeys(t, s, "documents/"); len(keys) != 1 {
		t.Fatalf("documents = %v", keys)
	}
	atts := attempts(t, s, sha)
	if len(atts) != 2 || atts[0].N != 1 || atts[0].Verdict != verify.Fail || atts[1].N != 2 || atts[1].Verdict != verify.Warn {
		t.Fatalf("attempts = %+v", atts)
	}
	if recs := records(t, s, sha); len(recs) != 1 || recs[0].ID != intake.RecordID(sha, 1) {
		t.Fatalf("records = %+v", recs)
	}
	if o, ok := outcome(t, s, sha); !ok || o.Disposition != intake.DispositionVerified || o.Attempts != 2 {
		t.Fatalf("outcome = %+v, %v", o, ok)
	}
	es, err := s.Entries(ctx, runID(sha), 0)
	if err != nil {
		t.Fatal(err)
	}
	turns, results := 0, map[string]int{}
	callTurn := 0
	for _, e := range es {
		switch e.Kind {
		case agent.EntryTurn:
			turns++
			callTurn = turns
		case agent.EntryResult:
			var re agent.ResultEntry
			if err := json.Unmarshal(e.Payload, &re); err != nil {
				t.Fatal(err)
			}
			results[fmt.Sprintf("%d/%d", callTurn, re.Call)]++
		}
	}
	if turns != 3 {
		t.Fatalf("%d turns recorded, want 3", turns)
	}
	for k, n := range results {
		if n != 1 {
			t.Fatalf("call %s has %d results", k, n)
		}
	}
	if len(results) != 3 {
		t.Fatalf("results = %v, want one per call", results)
	}
}

// TestCrashRecovery is TEST-5 on the intake agent (the slice's steps 3 and
// 4): the activation is stopped at every commit, before it lands and after
// it lands but before its result returns (the submission committed, its
// acknowledgement lost), the process restarts on the same store, and a new
// activation continues the run. The submission is never duplicated and no
// work is lost.
func TestCrashRecovery(t *testing.T) {
	needPoppler(t)
	pdfDir := t.TempDir()
	pdf := writePDF(t, pdfDir, synthetic(t, "keelson-vi"))
	reg := registry(t, intake.Options{})

	clk := &clock{now: fixedNow}
	s := openFile(t, t.TempDir(), clk)
	sha := createRun(t, s, pdf)
	cnt := &counting{Store: s}
	out, err := activate(t, cnt, s, reg, sha, ownerA, &scripted{script: sliceScript(t)}, nil, agent.Budgets{})
	if err != nil || out.Status != store.StatusSucceeded {
		t.Fatalf("uninterrupted run = %+v, %v", out, err)
	}
	checkSlice(t, s, sha)
	_ = s.Close()
	total := cnt.n
	if total < 6 {
		t.Fatalf("the run made %d commits; the test expects at least 6", total)
	}

	for at := 1; at <= total; at++ {
		for _, landing := range []bool{false, true} {
			t.Run(fmt.Sprintf("commit-%d/landing-%v", at, landing), func(t *testing.T) {
				t.Parallel()
				clk := &clock{now: fixedNow}
				dir := t.TempDir()
				s := openFile(t, dir, clk)
				sha := createRun(t, s, pdf)
				_, err := activate(t, &crashing{Store: s, at: at, landing: landing}, s, reg, sha, ownerA, &scripted{script: sliceScript(t)}, nil, agent.Budgets{})
				if !errors.Is(err, agent.ErrStoreFailure) {
					_ = s.Close()
					t.Fatalf("activation stopped at commit %d = %v, want a store failure", at, err)
				}
				// The worker never returns: the process restarts on the
				// same directory, and the host rereads the run record.
				_ = s.Close()
				clk.Advance(lease + time.Second)
				s = openFile(t, dir, clk)
				defer s.Close()
				r, err := s.Run(context.Background(), runID(sha))
				if err != nil {
					t.Fatal(err)
				}
				if !r.Status.Terminal() {
					out, err := activate(t, s, s, reg, sha, ownerB, &scripted{script: sliceScript(t)}, nil, agent.Budgets{})
					if err != nil || out.Status != store.StatusSucceeded || out.Reason.Code != agent.ReasonChanged {
						t.Fatalf("continuing activation = %+v, %v", out, err)
					}
				} else if at != total || !landing {
					t.Fatalf("run is %s after commit %d of %d stopped (landed %v)", r.Status, at, total, landing)
				}
				checkSlice(t, s, sha)
			})
		}
	}
}

// submissionPause pauses owner A's commit that carries the verified
// submission (its records), at one pause point.
type submissionPause struct {
	store.Store
	at      store.PausePoint
	carries atomic.Bool
	armed   atomic.Bool
	paused  chan struct{}
	resume  chan struct{}
}

func newSubmissionPause(at store.PausePoint) *submissionPause {
	p := &submissionPause{at: at, paused: make(chan struct{}), resume: make(chan struct{})}
	p.armed.Store(true)
	return p
}

// Commit marks A's commit that writes records, so the store's pause hook,
// called inside that commit, knows to pause it.
func (p *submissionPause) Commit(ctx context.Context, f store.Fence, ops []store.Op) error {
	for _, op := range ops {
		if op.PutObject != nil && strings.HasPrefix(op.PutObject.Key, "records/") {
			p.carries.Store(true)
		}
	}
	defer p.carries.Store(false)
	return p.Store.Commit(ctx, f, ops) //nolint:wrapcheck // a transparent wrapper
}

func (p *submissionPause) hook(_ context.Context, at store.PausePoint, f store.Fence) {
	if at == p.at && f.Owner == ownerA && p.carries.Load() && p.armed.CompareAndSwap(true, false) {
		close(p.paused)
		<-p.resume
	}
}

const hangFor = 30 * time.Second

func within[T any](t testing.TB, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(hangFor):
		t.Fatalf("%s did not return within %v", what, hangFor)
		var zero T
		return zero
	}
}

type result struct {
	out agent.Outcome
	err error
}

// TestStaleOwner is TEST-6 on the intake agent (the slice's step 5):
// activation A is paused in the commit of its verified submission, before
// the commit and inside it after reading ownership, while B takes the
// instance over and continues the run. Either serialization is valid; none
// of A's fenced writes (the attempt and records, the result entry, the
// checkpoint and the run's ending) may land after B's transfer committed,
// and the submission is stored exactly once.
func TestStaleOwner(t *testing.T) {
	needPoppler(t)
	pdf := writePDF(t, t.TempDir(), synthetic(t, "keelson-vi"))
	for _, at := range []store.PausePoint{store.PauseBeforeCommit, store.PauseAfterOwnershipRead} {
		t.Run(at.String(), func(t *testing.T) {
			reg := registry(t, intake.Options{})
			clk := &clockSafe{now: fixedNow}
			p := newSubmissionPause(at)
			s := openFileSafe(t, t.TempDir(), clk, filestore.WithPauseHook(p.hook))
			defer s.Close()
			p.Store = s
			sha := createRun(t, s, pdf)

			aDone := make(chan result, 1)
			go func() {
				out, err := activate(t, p, s, reg, sha, ownerA, &scripted{script: sliceScript(t)}, nil, agent.Budgets{})
				aDone <- result{out, err}
			}()
			select {
			case <-p.paused:
			case r := <-aDone:
				t.Fatalf("A finished (%+v, %v) without pausing in its submission's commit", r.out, r.err)
			case <-time.After(hangFor):
				t.Fatal("A did not reach the pause point")
			}

			// A's lease runs out; B claims the instance.
			clk.Advance(lease + time.Second)
			type claimed struct {
				own store.Ownership
				err error
			}
			bClaim := make(chan claimed, 1)
			go func() {
				own, err := s.Claim(context.Background(), runID(sha), ownerB, lease)
				bClaim <- claimed{own, err}
			}()
			var b claimed
			bFirst := false
			select {
			case b = <-bClaim:
				bFirst = true
			case <-time.After(300 * time.Millisecond):
			}
			if at == store.PauseBeforeCommit && !bFirst {
				t.Fatal("B's claim waited for a commit that had not started")
			}

			if bFirst {
				t.Log("B's claim committed while A was paused")
				if b.err != nil {
					t.Fatalf("B's claim: %v", b.err)
				}
				// B continues the run while A is still paused.
				ctrl := agent.NewController(s, reg)
				out, err := ctrl.Activate(context.Background(), agent.ActivationInput{
					Scope: store.Scope{Kind: store.ScopeRun, ID: runID(sha)}, Fence: b.own.Fence(), Namespace: namespace,
					Client: modelClient(t, &scripted{script: sliceScript(t)}),
				})
				if err != nil || out.Status != store.StatusSucceeded {
					t.Fatalf("B = %+v, %v", out, err)
				}
				close(p.resume)
				a := within(t, aDone, "A's activation")
				if !errors.Is(a.err, agent.ErrOwnershipLost) {
					t.Fatalf("A = %+v, %v; want ownership lost", a.out, a.err)
				}
			} else {
				t.Log("B's claim waited for A's commit")
				// B's claim is serialized behind A's commit, which lands
				// and ends the run; B's claim then follows.
				close(p.resume)
				a := within(t, aDone, "A's activation")
				b = within(t, bClaim, "B's claim")
				switch {
				case a.err == nil && a.out.Status == store.StatusSucceeded:
				case errors.Is(a.err, agent.ErrOwnershipLost):
					t.Fatal("A's commit was rejected although B's claim waited for it; B never continued the run")
				default:
					t.Fatalf("A = %+v, %v", a.out, a.err)
				}
				if b.err != nil {
					t.Fatalf("B's claim after A's commit: %v", b.err)
				}
				_, err := agent.NewController(s, reg).Activate(context.Background(), agent.ActivationInput{
					Scope: store.Scope{Kind: store.ScopeRun, ID: runID(sha)}, Fence: b.own.Fence(), Namespace: namespace,
				})
				if !errors.Is(err, agent.ErrNotActivatable) {
					t.Fatalf("B on the finished run: %v, want not activatable", err)
				}
			}
			checkSlice(t, s, sha)
			// The last entry is the run's single outcome: nothing of A's
			// landed after it.
			es, err := s.Entries(context.Background(), runID(sha), 0)
			if err != nil {
				t.Fatal(err)
			}
			outcomes := 0
			for _, e := range es {
				if e.Kind == agent.EntryOutcome {
					outcomes++
				}
			}
			if outcomes == 0 || es[len(es)-1].Kind != agent.EntryOutcome {
				t.Fatalf("the execution record ends with %s", es[len(es)-1].Kind)
			}
			if bFirst && outcomes != 1 {
				t.Fatalf("%d outcome entries, want B's alone", outcomes)
			}
		})
	}
}

// clockSafe is a clock read from several goroutines.
type clockSafe struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clockSafe) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clockSafe) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func openFileSafe(t testing.TB, dir string, clk *clockSafe, opts ...filestore.Option) store.Store {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the file store does not support Windows")
	}
	s, err := filestore.Open(filepath.Clean(dir), append([]filestore.Option{filestore.WithClock(clk.Now)}, opts...)...)
	if err != nil {
		t.Fatalf("filestore.Open: %v", err)
	}
	return s
}
