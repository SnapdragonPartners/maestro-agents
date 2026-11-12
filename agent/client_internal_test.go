package agent

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// TestCloneRequestSharesNothing: the baseline an attempt is compared with
// is equal to the request, and editing the request in place, through any
// slice, map or pointer it holds, leaves the baseline unchanged (ADR 0014,
// item 3).
func TestCloneRequestSharesNothing(t *testing.T) {
	build := func() llms.ChatRequest {
		temp := float32(0.5)
		return llms.ChatRequest{
			System: []llms.ContentPart{{Type: llms.ContentText, Text: "sys"}},
			Messages: []llms.Message{
				{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: "hi"}}},
				{Role: llms.RoleAssistant, Content: []llms.ContentPart{{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c1", Name: "t", Parameters: json.RawMessage(`{"a":1}`)}}}},
			},
			Tools:          []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			MaxTokens:      10,
			Temperature:    &temp,
			Metadata:       map[string]string{"k": "v"},
			Reasoning:      &llms.ReasoningConfig{BudgetTokens: 5},
			ProviderParams: map[llms.ProviderID]map[string]any{"fake": {"x": []any{1.0}}},
		}
	}
	edits := map[string]func(*llms.ChatRequest){
		"system text":     func(r *llms.ChatRequest) { r.System[0].Text = "x" },
		"message text":    func(r *llms.ChatRequest) { r.Messages[0].Content[0].Text = "x" },
		"tool call":       func(r *llms.ChatRequest) { r.Messages[1].Content[0].ToolCall.Name = "x" },
		"tool parameters": func(r *llms.ChatRequest) { r.Messages[1].Content[0].ToolCall.Parameters[1] = 'x' },
		"tool schema":     func(r *llms.ChatRequest) { r.Tools[0].InputSchema[0] = '[' },
		"temperature":     func(r *llms.ChatRequest) { *r.Temperature = 1 },
		"metadata":        func(r *llms.ChatRequest) { r.Metadata["k"] = "x" },
		"reasoning":       func(r *llms.ChatRequest) { r.Reasoning.BudgetTokens = 6 },
		"provider params": func(r *llms.ChatRequest) { r.ProviderParams["fake"]["x"].([]any)[0] = 2.0 }, //nolint:forcetypeassert // the fixture's own value
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			req := build()
			clone := cloneRequest(&req)
			if !reflect.DeepEqual(clone, req) {
				t.Fatal("the clone differs from the request")
			}
			edit(&req)
			if reflect.DeepEqual(clone, req) || !reflect.DeepEqual(clone, build()) {
				t.Fatal("editing the request changed its clone")
			}
		})
	}
}
