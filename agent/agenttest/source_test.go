package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/agent/agenttest"
)

func call(name string) *agent.ServedCall {
	return &agent.ServedCall{ToolCall: llms.ToolCall{ID: "1", Name: name}}
}

func TestSource(t *testing.T) {
	ctx := context.Background()
	var zero agenttest.Source
	if defs, err := zero.Tools(ctx); err != nil || len(defs) != 0 {
		t.Fatalf("zero Tools = %v, %v", defs, err)
	}
	s := new(agenttest.Source).
		Add(llms.ToolDefinition{Name: "b", InputSchema: json.RawMessage(`{"type":"object"}`)}, agenttest.Text("bee")).
		Add(llms.ToolDefinition{Name: "a"}, agenttest.Sequence(agenttest.Text("one"), agenttest.Text("two")))
	defs, err := s.Tools(ctx)
	if err != nil || len(defs) != 2 || defs[0].Name != "a" || defs[1].Name != "b" || defs[0].InputSchema != nil {
		t.Fatalf("Tools = %+v, %v", defs, err)
	}
	for _, want := range []string{"one", "two", "two"} {
		if res, err := s.Call(ctx, call("a")); err != nil || res.Content != want {
			t.Fatalf("Call(a) = %+v, %v; want %q", res, err, want)
		}
	}
	s.Remove("b")
	if _, err := s.Call(ctx, call("b")); !errors.Is(err, agent.ErrSourceError) {
		t.Fatalf("Call(removed) = %v, want ErrSourceError", err)
	}
	if got := s.Calls(); len(got) != 4 || got[3].ToolCall.Name != "b" {
		t.Fatalf("Calls = %+v", got)
	}
	boom := errors.New("boom")
	s.FailList(boom)
	if _, err := s.Tools(ctx); !errors.Is(err, boom) {
		t.Fatalf("Tools after FailList = %v", err)
	}
	s.FailList(nil)
	s.Add(llms.ToolDefinition{Name: "f"}, agenttest.Fail(agent.ErrSourceUnreachable))
	if _, err := s.Call(ctx, call("f")); !errors.Is(err, agent.ErrSourceUnreachable) {
		t.Fatalf("Call(f) = %v", err)
	}
}

func TestSequenceEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Sequence() did not panic")
		}
	}()
	agenttest.Sequence()
}
