package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/store"
	"github.com/SnapdragonPartners/maestro-agents/store/memstore"
)

// credScript calls fetch, then finishes.
var credScript = []llms.ChatResponse{
	callsResp(tc("c1", "fetch", `{}`)),
	callsResp(tc("c2", "finish", `{}`)),
}

// credLog records what the credential provider, the policy and the tool
// saw.
type credLog struct {
	provider []agent.RunContext
	policy   []agent.RunContext
	tool     []agent.RunContext
	results  []string
	mu       sync.Mutex
}

// credType is the ledger type with a fetch tool that asks for the "api"
// credential and reports what it got, rendered with fmt (which must not
// show it).
func credType(log *credLog) *agent.Type[ledgerData] {
	var execs execLog
	tools := ledgerTools(&execs)
	tools = append(tools, agent.Tool[ledgerData]{
		Definition: llms.ToolDefinition{Name: "fetch", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Effect:     agent.EffectRead,
		Execute: func(ctx context.Context, c *agent.Call[ledgerData]) (agent.Result[ledgerData], error) {
			log.mu.Lock()
			log.tool = append(log.tool, c.RunContext())
			log.mu.Unlock()
			s, err := c.Credential(ctx, "api")
			res := fmt.Sprintf("secret %v revealed-len %d", s, len(s.Reveal()))
			if err != nil {
				res = "error: " + err.Error()
				if errors.Is(err, agent.ErrNoCredential) {
					res = "no credential"
				}
			}
			log.mu.Lock()
			log.results = append(log.results, res)
			log.mu.Unlock()
			return agent.TextResult[ledgerData](res), nil
		},
	})
	return ledgerType(&execs, tools...)
}

func credActivate(c *agent.Controller, s store.Store, t *testing.T, principal string, maxSteps int) agent.Outcome {
	t.Helper()
	out, err := c.Activate(context.Background(), agent.ActivationInput{
		Fence: claim(t, s), Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Namespace: ns,
		Client: modelClient(scripted(credScript)), Principal: principal, MaxSteps: maxSteps,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	return out
}

func TestCredentialsThroughTheRunContext(t *testing.T) {
	log := &credLog{}
	s := memstore.New()
	provider := func(_ context.Context, rc *agent.RunContext, name string) (agent.Secret, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.provider = append(log.provider, *rc)
		if name != "api" || rc.Principal != "alice" {
			return agent.Secret{}, agent.ErrNoCredential
		}
		return agent.NewSecret("s3cret-value"), nil
	}
	policy := func(_ context.Context, req *agent.PolicyRequest) (agent.Decision, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		log.policy = append(log.policy, req.Run)
		return agent.Decision{Verdict: agent.VerdictAllow}, nil
	}
	c := agent.NewController(s, registry(t, credType(log)), agent.WithCredentials(provider), agent.WithPolicy(policy))
	createLedgerRun(t, s)
	// The run starts with alice; a later activation naming another
	// principal does not change it (ADR 0016, item 12).
	if out := credActivate(c, s, t, "alice", 1); out.Status != store.StatusRunnable {
		t.Fatalf("first activation = %+v", out)
	}
	if out := credActivate(c, s, t, "mallory", 0); !out.Status.Terminal() {
		t.Fatalf("second activation = %+v", out)
	}
	want := agent.RunContext{Scope: store.Scope{Kind: store.ScopeRun, ID: "r1"}, Instance: inst, AgentType: "ledger", Principal: "alice"}
	if len(log.provider) != 1 || log.provider[0] != want || len(log.tool) != 1 || log.tool[0] != want {
		t.Fatalf("provider saw %+v, tool saw %+v", log.provider, log.tool)
	}
	if len(log.policy) == 0 {
		t.Fatal("policy never called")
	}
	for _, rc := range log.policy {
		if rc != want {
			t.Fatalf("policy saw %+v, want %+v", rc, want)
		}
	}
	if len(log.results) != 1 || log.results[0] != "secret [redacted] revealed-len 12" {
		t.Fatalf("tool results = %v", log.results)
	}
	// The principal is recorded with the run's start; the credential is
	// recorded nowhere.
	entries, err := s.Entries(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	var started agent.RunStarted
	for _, e := range entries {
		if strings.Contains(string(e.Payload), "s3cret-value") {
			t.Fatalf("entry %d records the credential", e.Seq)
		}
		if e.Kind == agent.EntryRunStarted {
			started = payload[agent.RunStarted](t, e)
		}
	}
	if started.Principal != "alice" {
		t.Fatalf("run_started principal = %q", started.Principal)
	}
	ck, ok, err := s.LatestCheckpoint(context.Background(), inst)
	if err != nil || !ok || !strings.Contains(string(ck.State), `"principal":"alice"`) {
		t.Fatalf("checkpoint = %s, %v, %v", ck.State, ok, err)
	}
}

func TestNoCredentialProvider(t *testing.T) {
	for name, opts := range map[string][]agent.Option{
		"default":  nil,
		"nil sets": {agent.WithCredentials(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			log := &credLog{}
			s := memstore.New()
			c := agent.NewController(s, registry(t, credType(log)), opts...)
			createLedgerRun(t, s)
			credActivate(c, s, t, "", 0)
			if len(log.results) != 1 || log.results[0] != "no credential" {
				t.Fatalf("tool results = %v", log.results)
			}
			if log.tool[0].Principal != "" {
				t.Fatalf("principal = %q", log.tool[0].Principal)
			}
		})
	}
}

func TestInvalidPrincipalIsAConfigError(t *testing.T) {
	log := &credLog{}
	s := memstore.New()
	c := agent.NewController(s, registry(t, credType(log)))
	createLedgerRun(t, s)
	out := credActivate(c, s, t, "bad\xff", 0)
	if out.Status != store.StatusFailed || out.Reason.Code != agent.ReasonConfigError {
		t.Fatalf("outcome = %+v", out)
	}
	if len(log.tool) != 0 {
		t.Fatal("a tool ran under an invalid principal")
	}
}
