package providerapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// KiroRuntime describes one account and model for a single network attempt.
// A dedicated system field and the Responses API are independent capabilities.
type KiroRuntime struct {
	PromptCaching KiroPromptCaching
	replay        kiroReplayScope
	Profile       string
	Responses     bool
	SystemField   bool
	ModelSchema   json.RawMessage
}

func (runtime KiroRuntime) modelFields(root ojson.Result) (map[string]any, error) {
	props := ojson.GetBytes(runtime.ModelSchema, "properties")
	fields := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "max_tokens"} {
		v, p := root.Get(key), props.Get(key)
		if !v.Exists() {
			continue
		}
		if runtime.Responses || !p.IsObject() {
			// Messages requires max_tokens even on providers without a wire setting.
			if key == "max_tokens" {
				continue
			}
			return nil, fmt.Errorf("Kiro model does not advertise %s on this endpoint", key)
		}
		if v.Type != ojson.Number || !kiroSchemaAllows(p, v) {
			return nil, fmt.Errorf("invalid Kiro %s for this model", key)
		}
		fields[key] = json.RawMessage(v.Raw)
	}
	if v := root.Get("thinking"); v.Exists() && v.Type != ojson.Null {
		if !v.IsObject() || v.Get("type").Type != ojson.String {
			return nil, fmt.Errorf("invalid Kiro thinking configuration")
		}
		supported := props.Get("thinking.properties")
		out := map[string]any{}
		for key, value := range v.Map() {
			switch key {
			case "type":
				if value.String() == "disabled" && !supported.IsObject() {
					continue
				}
			case "display":
			case "budget_tokens":
				if runtime.Responses {
					return nil, fmt.Errorf("Kiro CreateResponse cannot represent thinking.budget_tokens")
				}
			default:
				return nil, fmt.Errorf("unsupported Kiro thinking option")
			}
			schema := supported.Get(key)
			if !schema.IsObject() || !kiroSchemaAllows(schema, value) {
				return nil, fmt.Errorf("unsupported Kiro thinking.%s for this model", key)
			}
			out[key] = json.RawMessage(value.Raw)
		}
		if len(out) > 0 {
			fields["thinking"] = out
		}
	}
	if cfg := root.Get("output_config"); cfg.Exists() && cfg.Type != ojson.Null {
		if !cfg.IsObject() {
			return nil, fmt.Errorf("invalid Kiro output configuration")
		}
		for key := range cfg.Map() {
			if key != "effort" {
				return nil, fmt.Errorf("unsupported Kiro output configuration option")
			}
		}
	}
	if v := root.Get("output_config.effort"); v.Exists() {
		target := "output_config"
		schema := props.Get(target + ".properties.effort")
		if !schema.IsObject() {
			target = "reasoning"
			schema = props.Get(target + ".properties.effort")
		}
		if !schema.IsObject() || !kiroSchemaAllows(schema, v) {
			return nil, fmt.Errorf("unsupported Kiro reasoning effort for this model")
		}
		fields[target] = map[string]any{"effort": v.String()}
	}
	return fields, nil
}

func kiroSchemaAllows(schema, value ojson.Result) bool {
	switch schema.Get("type").String() {
	case "integer", "number":
		if value.Type != ojson.Number || schema.Get("type").String() == "integer" && value.Float() != float64(value.Int()) {
			return false
		}
		if min := schema.Get("minimum"); min.Exists() && value.Float() < min.Float() {
			return false
		}
		if max := schema.Get("maximum"); max.Exists() && value.Float() > max.Float() {
			return false
		}
	case "string":
		if value.Type != ojson.String {
			return false
		}
	case "": // Some catalogs describe a string using only its enum.
		if !schema.Get("enum").IsArray() {
			return false
		}
	default:
		return false
	}
	if values := schema.Get("enum"); values.IsArray() {
		for _, v := range values.Array() {
			if v.Type == value.Type && v.String() == value.String() {
				return true
			}
		}
		return false
	}
	return true
}

func (c *kiroConversation) responsesPayload(profile string, fields map[string]any) map[string]any {
	input := []any{}
	for _, turn := range append(c.history, kiroTurn{User: c.current}) {
		if turn.User != nil {
			input = turn.User.responsesInput(input)
		} else if turn.Assistant != nil {
			input = turn.Assistant.responsesInput(input)
		}
	}
	payload := map[string]any{
		"model": c.model, "input": input, "origin": "AI_EDITOR", "stream": true,
		"metadata": map[string]string{
			"kiro:agent_mode": "vibe", "kiro:conversation_id": c.id, "kiro:agent_continuation_id": c.continuation,
		},
	}
	if profile != "" {
		payload["profileArn"] = profile
	}
	if c.current.Context != nil && len(c.current.Context.Tools) > 0 {
		tools := []any{}
		for _, t := range c.current.Context.Tools {
			s := t.Specification
			if s == nil {
				continue
			}
			tools = append(tools, map[string]any{"type": "function", "name": s.Name, "description": s.Description, "parameters": s.InputSchema.JSON})
		}
		payload["tools"] = tools
	}
	reasoning := map[string]any{}
	if t, ok := fields["thinking"].(map[string]any); ok {
		if v := t["type"]; v != nil {
			reasoning["type"] = v
		}
		if v := t["display"]; v != nil {
			reasoning["summary"] = v
		}
	}
	for _, key := range []string{"output_config", "reasoning"} {
		if t, ok := fields[key].(map[string]any); ok {
			reasoning["effort"] = t["effort"]
		}
	}
	if len(reasoning) > 0 {
		payload["reasoning"] = reasoning
	}
	return payload
}

func (u *kiroUser) responsesInput(input []any) []any {
	if u.Context != nil {
		for _, result := range u.Context.Results {
			parts := make([]string, 0, len(result.Content))
			for _, c := range result.Content {
				parts = append(parts, c["text"])
			}
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": result.ID, "output": strings.Join(parts, "\n"),
			})
		}
	}
	var content any = u.Content
	if len(u.Images)+len(u.Documents) > 0 {
		parts := []any{}
		if u.Content != "" {
			parts = append(parts, map[string]string{"type": "input_text", "text": u.Content})
		}
		for _, im := range u.Images {
			parts = append(parts, map[string]string{"type": "input_image", "image_url": "data:image/" + im.Format + ";base64," + im.Source.Bytes})
		}
		for _, doc := range u.Documents {
			for mime, f := range kiroDocumentFormats {
				if f == doc.Format {
					parts = append(parts, map[string]string{"type": "input_file", "filename": doc.Name + "." + f, "file_data": "data:" + mime + ";base64," + doc.Source.Bytes})
					break
				}
			}
		}
		content = parts
	}
	if u.Content != "" || len(u.Images)+len(u.Documents) > 0 {
		input = append(input, map[string]any{"role": "user", "content": content})
	}
	return input
}

// CreateResponse has no status field on function_call_output. Keep tool output
// verbatim and let the attempt diagnostic report the omitted GAR error status.
func (c *kiroConversation) hasToolResultError() bool {
	for _, turn := range append(c.history, kiroTurn{User: c.current}) {
		if turn.User != nil && turn.User.Context != nil {
			for _, result := range turn.User.Context.Results {
				if result.Status == "error" {
					return true
				}
			}
		}
	}
	return false
}

func (a *kiroAssistant) responsesInput(input []any) []any {
	if a.Reasoning != nil {
		encoded, _ := json.Marshal(a.Reasoning)
		input = append(input, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": string(encoded)})
	}
	if a.Content != "" {
		input = append(input, map[string]string{"role": "assistant", "content": a.Content})
	}
	for _, call := range a.Tools {
		input = append(input, map[string]string{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Input)})
	}
	return input
}
