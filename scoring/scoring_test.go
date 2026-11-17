package scoring_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/scoring"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
)

// fakeHost reports a scripted run per input path.
type fakeHost struct {
	reports map[string]standalone.Report
	errs    map[string]error
	ran     []string
	cancel  func() // called after the first input
}

func (h *fakeHost) RunInput(_ context.Context, path string) (standalone.Report, error) {
	h.ran = append(h.ran, path)
	if h.cancel != nil {
		h.cancel()
	}
	if err := h.errs[path]; err != nil {
		return standalone.Report{}, err
	}
	return h.reports[path], nil
}
func (h *fakeHost) Store() store.Reader { return nil }
func (h *fakeHost) Namespace() string   { return "ns" }

func succeeded(tokens int64, turns int) standalone.Report {
	return standalone.Report{Status: store.StatusSucceeded, Activated: true, Consumed: &agent.Consumption{Tokens: tokens, Turns: turns}}
}

// clock advances a second per call.
func clock() func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func items(vs ...scoring.Verdict) []scoring.Item {
	out := make([]scoring.Item, len(vs))
	for i, v := range vs {
		out[i] = scoring.Item{Key: string(v), Verdict: v}
	}
	return out
}

func TestRunScoresEachInputInOrder(t *testing.T) {
	recorded := succeeded(5, 1)
	recorded.Activated = false
	h := &fakeHost{
		reports: map[string]standalone.Report{
			"a.pdf": succeeded(100, 2), "b.pdf": succeeded(50, 3), "c.pdf": succeeded(10, 1), "e.pdf": recorded,
		},
		errs: map[string]error{"d.pdf": errors.New("cannot read")},
	}
	cmp := scoring.ComparatorFunc(func(_ context.Context, out *scoring.Output) ([]scoring.Item, error) {
		if out.Namespace != "ns" {
			t.Errorf("namespace %q", out.Namespace)
		}
		switch out.Input.Path {
		case "a.pdf", "e.pdf":
			return items(scoring.Match, scoring.Missed), nil
		case "b.pdf":
			return items(scoring.Exception), nil
		}
		return nil, scoring.ErrNoAnswer
	})
	in := []scoring.Input{{Path: "a.pdf"}, {Path: "b.pdf"}, {Path: "c.pdf"}, {Path: "d.pdf"}, {Path: "e.pdf"}}
	rep := scoring.Run(context.Background(), h, in, cmp, clock())
	if strings.Join(h.ran, ",") != "a.pdf,b.pdf,c.pdf,d.pdf,e.pdf" {
		t.Fatalf("ran %v", h.ran)
	}
	tot := rep.Totals
	if tot.Inputs != 5 || tot.Uncovered != 1 || tot.Errors != 1 || tot.Recorded != 1 || tot.Tokens != 165 || tot.Turns != 7 {
		t.Fatalf("totals %+v", tot)
	}
	if tot.Verdicts[scoring.Match] != 2 || tot.Verdicts[scoring.Missed] != 2 || tot.Verdicts[scoring.Exception] != 1 || tot.Verdicts[scoring.Mismatch] != 0 {
		t.Fatalf("verdicts %+v", tot.Verdicts)
	}
	if rep.Results[0].Wall != time.Second || tot.Wall != 5*time.Second {
		t.Fatalf("wall %v / %v", rep.Results[0].Wall, tot.Wall)
	}
	if !rep.Results[2].Uncovered || rep.Results[3].Error == "" || !rep.Results[4].Recorded {
		t.Fatalf("results %+v", rep.Results)
	}
	if !rep.Complete || !rep.Failed() {
		t.Fatal("a host error fails the scoring run")
	}
}

func TestFailingVerdicts(t *testing.T) {
	for v, failing := range map[scoring.Verdict]bool{
		scoring.Match: false, scoring.Missed: false, scoring.Exception: false,
		scoring.Mismatch: true, scoring.Hallucination: true,
	} {
		h := &fakeHost{reports: map[string]standalone.Report{"a.pdf": succeeded(1, 1)}}
		cmp := scoring.ComparatorFunc(func(context.Context, *scoring.Output) ([]scoring.Item, error) { return items(v), nil })
		rep := scoring.Run(context.Background(), h, []scoring.Input{{Path: "a.pdf"}}, cmp, nil)
		if rep.Failed() != failing || v.Failing() != failing {
			t.Errorf("%s: Failed = %v, want %v", v, rep.Failed(), failing)
		}
	}
}

func TestComparatorErrors(t *testing.T) {
	h := &fakeHost{reports: map[string]standalone.Report{"a.pdf": succeeded(1, 1), "b.pdf": succeeded(1, 1)}}
	cmp := scoring.ComparatorFunc(func(_ context.Context, out *scoring.Output) ([]scoring.Item, error) {
		if out.Input.Path == "a.pdf" {
			return nil, errors.New("store gone")
		}
		return []scoring.Item{{Key: "x", Verdict: "close enough"}}, nil
	})
	rep := scoring.Run(context.Background(), h, []scoring.Input{{Path: "a.pdf"}, {Path: "b.pdf"}}, cmp, nil)
	if !strings.Contains(rep.Results[0].Error, "store gone") {
		t.Fatalf("a: %+v", rep.Results[0])
	}
	if !strings.Contains(rep.Results[1].Error, "unknown verdict") || rep.Results[1].Items != nil {
		t.Fatalf("b: %+v", rep.Results[1])
	}
	if !rep.Failed() || rep.Totals.Errors != 2 {
		t.Fatalf("totals %+v", rep.Totals)
	}
}

func TestCancelStopsAfterTheCurrentInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &fakeHost{reports: map[string]standalone.Report{"a.pdf": succeeded(1, 1)}, cancel: cancel}
	cmp := scoring.ComparatorFunc(func(context.Context, *scoring.Output) ([]scoring.Item, error) { return items(scoring.Match), nil })
	rep := scoring.Run(ctx, h, []scoring.Input{{Path: "a.pdf"}, {Path: "b.pdf"}}, cmp, nil)
	if len(h.ran) != 1 || len(rep.Results) != 1 || rep.Complete || !rep.Failed() {
		t.Fatalf("ran %v, complete %v", h.ran, rep.Complete)
	}
}

func TestWriteReport(t *testing.T) {
	recorded := succeeded(5, 1)
	recorded.Activated = false
	h := &fakeHost{reports: map[string]standalone.Report{"dir/a.pdf": succeeded(100, 2), "dir/b.pdf": recorded, "dir/c.pdf": succeeded(7, 1)}}
	cmp := scoring.ComparatorFunc(func(_ context.Context, out *scoring.Output) ([]scoring.Item, error) {
		switch out.Input.Path {
		case "dir/a.pdf":
			return []scoring.Item{
				{Key: "Owner / Fund", Want: "2026-03-31 10 USD", Got: "2026-03-31 11 USD", Verdict: scoring.Mismatch, Reason: "differs in value"},
				{Key: "Owner / Other", Verdict: scoring.Match},
			}, nil
		case "dir/b.pdf":
			return items(scoring.Match), nil
		}
		return nil, scoring.ErrNoAnswer
	})
	rep := scoring.Run(context.Background(), h, []scoring.Input{{Path: "dir/a.pdf"}, {Path: "dir/b.pdf"}, {Path: "dir/c.pdf"}}, cmp, clock())
	var b bytes.Buffer
	if err := rep.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, want := range []string{
		"input", "a.pdf", "succeeded (recorded)", "succeeded (uncovered)", "total",
		`a.pdf: MISMATCH Owner / Fund (want "2026-03-31 10 USD", got "2026-03-31 11 USD"): differs in value`,
		"1 input(s) not in the answer key", "1 input(s) reported from runs already in the store", "FAIL",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "MATCH Owner / Other") {
		t.Errorf("matches are not listed:\n%s", text)
	}
	b.Reset()
	if err := rep.WriteJSON(&b); err != nil {
		t.Fatal(err)
	}
	var back scoring.Report
	if err := json.Unmarshal(b.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if back.Totals.Verdicts[scoring.Mismatch] != 1 || len(back.Results) != 3 || !back.Results[2].Uncovered {
		t.Fatalf("round trip %+v", back)
	}

	pass := scoring.Run(context.Background(), &fakeHost{reports: map[string]standalone.Report{"a.pdf": succeeded(1, 1)}}, []scoring.Input{{Path: "a.pdf"}},
		scoring.ComparatorFunc(func(context.Context, *scoring.Output) ([]scoring.Item, error) { return items(scoring.Match), nil }), nil)
	b.Reset()
	if err := pass.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "PASS") {
		t.Errorf("passing report:\n%s", b.String())
	}
}
