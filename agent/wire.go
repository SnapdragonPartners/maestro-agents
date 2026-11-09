package agent

import (
	"errors"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// The recorded form of maestro-llms' messages (ADR 0008, item 8). Every
// message, tool call and result the record stores goes through it, so what
// recovery reads back is byte for byte what the loop sent and received:
// tool-call arguments are kept as bytes, since encoding/json would compact a
// json.RawMessage (and refuse a malformed one), and every string is checked
// as valid UTF-8 where it enters (checkMessage, checkParts), since
// encoding/json would replace invalid bytes.

type wireCall struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Parameters        []byte `json:"parameters"`
	ProviderSignature []byte `json:"provider_signature,omitempty"`
}

type wireResult struct {
	ToolCallID string     `json:"tool_call_id"`
	Content    string     `json:"content,omitempty"`
	Parts      []wirePart `json:"parts,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

type wirePart struct {
	ToolCall        *wireCall            `json:"tool_call,omitempty"`
	ToolResult      *wireResult          `json:"tool_result,omitempty"`
	Reasoning       *llms.Reasoning      `json:"reasoning,omitempty"`
	Media           *llms.Media          `json:"media,omitempty"`
	Type            llms.ContentPartType `json:"type"`
	Text            string               `json:"text,omitempty"`
	CacheBreakpoint bool                 `json:"cache_breakpoint,omitempty"`
}

type wireMessage struct {
	Role    llms.Role  `json:"role"`
	Content []wirePart `json:"content"`
}

func toWireCall(c *llms.ToolCall) *wireCall {
	return &wireCall{
		ID: c.ID, Name: c.Name,
		Parameters:        append([]byte{}, c.Parameters...),
		ProviderSignature: append([]byte(nil), c.ProviderSignature...),
	}
}

func (w *wireCall) call() llms.ToolCall {
	c := llms.ToolCall{ID: w.ID, Name: w.Name, ProviderSignature: append([]byte(nil), w.ProviderSignature...)}
	if w.Parameters != nil {
		c.Parameters = append([]byte(nil), w.Parameters...)
	}
	return c
}

func toWireCalls(calls []llms.ToolCall) []wireCall {
	out := make([]wireCall, len(calls))
	for i := range calls {
		out[i] = *toWireCall(&calls[i])
	}
	return out
}

func fromWireCalls(calls []wireCall) []llms.ToolCall {
	out := make([]llms.ToolCall, len(calls))
	for i := range calls {
		out[i] = calls[i].call()
	}
	return out
}

func toWireParts(parts []llms.ContentPart) []wirePart {
	if parts == nil {
		return nil
	}
	out := make([]wirePart, len(parts))
	for i := range parts {
		p := &parts[i]
		w := wirePart{Type: p.Type, Text: p.Text, Reasoning: p.Reasoning, Media: p.Media, CacheBreakpoint: p.CacheBreakpoint}
		if p.ToolCall != nil {
			w.ToolCall = toWireCall(p.ToolCall)
		}
		if r := p.ToolResult; r != nil {
			w.ToolResult = &wireResult{ToolCallID: r.ToolCallID, Content: r.Content, Parts: toWireParts(r.Parts), IsError: r.IsError}
		}
		out[i] = w
	}
	return out
}

func fromWireParts(parts []wirePart) []llms.ContentPart {
	if parts == nil {
		return nil
	}
	out := make([]llms.ContentPart, len(parts))
	for i := range parts {
		w := &parts[i]
		p := llms.ContentPart{Type: w.Type, Text: w.Text, Reasoning: w.Reasoning, Media: w.Media, CacheBreakpoint: w.CacheBreakpoint}
		if w.ToolCall != nil {
			c := w.ToolCall.call()
			p.ToolCall = &c
		}
		if r := w.ToolResult; r != nil {
			p.ToolResult = &llms.ToolResult{ToolCallID: r.ToolCallID, Content: r.Content, Parts: fromWireParts(r.Parts), IsError: r.IsError}
		}
		out[i] = p
	}
	return out
}

func toWireMessage(m *llms.Message) wireMessage {
	return wireMessage{Role: m.Role, Content: toWireParts(m.Content)}
}

func (w *wireMessage) message() llms.Message {
	return llms.Message{Role: w.Role, Content: fromWireParts(w.Content)}
}

// checkMessage refuses a message the record could not keep exactly: a
// string that is not valid UTF-8.
func checkMessage(m *llms.Message) error {
	return checkStrings(m.Content)
}

func checkStrings(parts []llms.ContentPart) error {
	for i := range parts {
		if err := checkPartStrings(&parts[i]); err != nil {
			return fmt.Errorf("part %d: %w", i, err)
		}
	}
	return nil
}

func checkPartStrings(p *llms.ContentPart) error {
	valid := utf8.ValidString(string(p.Type)) && utf8.ValidString(p.Text)
	if c := p.ToolCall; c != nil {
		valid = valid && utf8.ValidString(c.ID) && utf8.ValidString(c.Name)
	}
	if r := p.Reasoning; r != nil {
		valid = valid && utf8.ValidString(string(r.Provider)) && utf8.ValidString(r.Text)
	}
	if m := p.Media; m != nil {
		valid = valid && utf8.ValidString(m.MediaType)
	}
	if r := p.ToolResult; r != nil {
		valid = valid && utf8.ValidString(r.ToolCallID) && utf8.ValidString(r.Content)
		if err := checkStrings(r.Parts); err != nil {
			return err
		}
	}
	if !valid {
		return errors.New("a string is not valid UTF-8")
	}
	return nil
}

// checkParts applies maestro-llms' rules for a tool result (its
// media.CheckToolResult, ADR-0041) before the result is recorded, so the
// loop never refuses a result already committed, and checks every string
// is valid UTF-8.
func checkParts(content string, parts []llms.ContentPart) error {
	if !utf8.ValidString(content) {
		return errors.New("result content is not valid UTF-8")
	}
	if err := checkStrings(parts); err != nil {
		return err
	}
	if len(parts) == 0 {
		return nil
	}
	if content != "" {
		return errors.New("result sets both Content and Parts")
	}
	for i := range parts {
		p := &parts[i]
		switch p.Type {
		case llms.ContentText:
			if p.Text == "" {
				return fmt.Errorf("part %d is an empty text part", i)
			}
		case llms.ContentImage, llms.ContentDocument:
			if err := checkMedia(p); err != nil {
				return fmt.Errorf("part %d: %w", i, err)
			}
		default:
			return fmt.Errorf("part %d has type %q; a result holds text, image and document parts only", i, p.Type)
		}
	}
	return nil
}

// checkMedia mirrors maestro-llms' media.Check: bytes present, a media type
// that parses as type/subtype, an image type for an image part and a
// non-image type for a document part.
func checkMedia(p *llms.ContentPart) error {
	if p.Media == nil {
		return fmt.Errorf("%s part has no media", p.Type)
	}
	if len(p.Media.Data) == 0 {
		return fmt.Errorf("%s part has no data", p.Type)
	}
	if strings.TrimSpace(p.Media.MediaType) == "" {
		return fmt.Errorf("%s part has no media type", p.Type)
	}
	base, _, err := mime.ParseMediaType(p.Media.MediaType)
	if err != nil || !strings.Contains(base, "/") {
		return fmt.Errorf("%s part has invalid media type %q", p.Type, p.Media.MediaType)
	}
	image := strings.HasPrefix(base, "image/")
	switch {
	case p.Type == llms.ContentImage && !image:
		return fmt.Errorf("image part has non-image media type %q", base)
	case p.Type == llms.ContentDocument && image:
		return fmt.Errorf("document part has image media type %q", base)
	}
	return nil
}

// checkRequest refuses a prompt's request the record could not keep
// exactly.
func checkRequest(req *llms.ChatRequest) error {
	if err := checkStrings(req.System); err != nil {
		return fmt.Errorf("system: %w", err)
	}
	if !utf8.ValidString(string(req.Purpose)) || !utf8.ValidString(string(req.ToolChoice.Type)) || !utf8.ValidString(req.ToolChoice.Name) {
		return errors.New("purpose or tool choice is not valid UTF-8")
	}
	if req.Reasoning != nil && !utf8.ValidString(string(req.Reasoning.Effort)) {
		return errors.New("reasoning effort is not valid UTF-8")
	}
	for k, v := range req.Metadata {
		if !utf8.ValidString(k) || !utf8.ValidString(v) {
			return errors.New("metadata holds a string that is not valid UTF-8")
		}
	}
	for p, params := range req.ProviderParams {
		if !utf8.ValidString(string(p)) || !validStrings(params) {
			return fmt.Errorf("provider params for %q hold a string that is not valid UTF-8", p)
		}
	}
	for i := range req.Messages {
		if err := checkMessage(&req.Messages[i]); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	return nil
}

// checkResponse refuses a response the record could not keep exactly.
func checkResponse(resp *llms.ChatResponse) error {
	if err := checkMessage(&resp.Message); err != nil {
		return err
	}
	if !utf8.ValidString(resp.Text) || !utf8.ValidString(string(resp.StopReason)) {
		return errors.New("text or stop reason is not valid UTF-8")
	}
	for i := range resp.ToolCalls {
		if c := &resp.ToolCalls[i]; !utf8.ValidString(c.ID) || !utf8.ValidString(c.Name) {
			return fmt.Errorf("tool call %d has an ID or name that is not valid UTF-8", i)
		}
	}
	return nil
}

// validStrings reports whether every string in a JSON-like value, map keys
// included, is valid UTF-8. Values it does not know (numbers, structs)
// pass: encoding/json then reports what it cannot encode.
func validStrings(v any) bool {
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x)
	case map[string]any:
		for k, e := range x {
			if !utf8.ValidString(k) || !validStrings(e) {
				return false
			}
		}
	case []any:
		for _, e := range x {
			if !validStrings(e) {
				return false
			}
		}
	}
	return true
}
