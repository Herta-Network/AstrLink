package providerapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKiroSystemMessagesKeepTheirPositionAndCacheBoundaries(t *testing.T) {
	const body = `{
	 "model":"m", "system":"top",
	 "messages":[
	  {"role":"system","content":[{"type":"text","text":"prefix","cache_control":{"type":"ephemeral"}}]},
	  {"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral"}}]},
	  {"role":"system","content":"update one"},
	  {"role":"assistant","content":"answer"},
	  {"role":"system","content":"update two"},
	  {"role":"user","content":"question"},
	  {"role":"system","content":[{"type":"text","text":"tail","cache_control":{"type":"ephemeral"}}]}
	 ]}`
	for _, dedicated := range []bool{false, true} {
		wire, losses, err := encodeCacheRequest(t, body, KiroRuntime{SystemField: dedicated, PromptCaching: KiroPromptCaching{SupportsPromptCaching: true}})
		if err != nil {
			t.Fatal(err)
		}
		history := wire.Get("conversationState.history").Array()
		prefix := "top\n\nprefix"
		if dedicated {
			if wire.Get("systemPrompt").String() != prefix {
				t.Fatalf("system field: %s", wire.Raw)
			}
		} else {
			if len(history) < 2 || history[0].Get("userInputMessage.content").String() != prefix || history[0].Get("userInputMessage.cachePoint.type").String() != "default" {
				t.Fatalf("historical prefix: %s", wire.Raw)
			}
			history = history[2:]
		}
		if len(history) != 2 || history[0].Get("userInputMessage.content").String() != "first\n\n<system-reminder>\nupdate one\n</system-reminder>" || history[1].Get("assistantResponseMessage.content").String() != "answer" {
			t.Fatalf("history reordered: %s", wire.Raw)
		}
		current := wire.Get("conversationState.currentMessage.userInputMessage")
		if current.Get("content").String() != "<system-reminder>\nupdate two\n</system-reminder>\n\nquestion\n\n<system-reminder>\ntail\n</system-reminder>" || current.Get("cachePoint.type").String() != "default" {
			t.Fatalf("current: %s", current.Raw)
		}
		if history[0].Get("userInputMessage.cachePoint").Exists() {
			t.Fatal("interior breakpoint moved past a system reminder")
		}
		found := false
		for _, loss := range losses {
			if loss.Path == "messages.1.content.0.cache_control" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing interior breakpoint diagnostic")
		}
	}
}

func TestKiroClaudeCodeToolSearchVisibility(t *testing.T) {
	const tools = `[{"name":"ToolSearch","input_schema":{"type":"object"}},{"name":"DeferredToolPlaceholder","defer_loading":true,"input_schema":{"type":"object"}},{"name":"ReadLater","defer_loading":true,"input_schema":{"type":"object","properties":{"value":{"const":9007199254740993}}}},{"name":"Hidden","defer_loading":true,"input_schema":{"type":"object"}}]`
	for _, tc := range []struct {
		name, call, resultID string
		failed, visible      bool
	}{
		{"successful search", "ToolSearch", "one", false, true},
		{"failed search", "ToolSearch", "one", true, false},
		{"mismatched result", "ToolSearch", "other", false, false},
		{"ordinary tool", "Ordinary", "one", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := []any{map[string]string{"role": "user", "content": "find it"},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "one", "name": tc.call, "input": map[string]string{"query": "select:ReadLater"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": tc.resultID, "is_error": tc.failed, "content": []any{map[string]string{"type": "tool_reference", "tool_name": "ReadLater"}, map[string]string{"type": "text", "text": "Hidden"}}}}},
				map[string]string{"role": "system", "content": "turn context"}}
			raw, _ := json.Marshal(map[string]any{"model": "m", "tools": json.RawMessage(tools), "messages": messages})
			wire, _, err := encodeCacheRequest(t, string(raw), KiroRuntime{})
			if err != nil {
				t.Fatal(err)
			}
			ctx := wire.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext")
			definitions := ctx.Get("tools")
			loaded := definitions.Get(`#(toolSpecification.name=="ReadLater").toolSpecification`)
			if loaded.Exists() != tc.visible || definitions.Get(`#(toolSpecification.name=="Hidden")`).Exists() || strings.Contains(definitions.Raw, "DeferredToolPlaceholder") || !definitions.Get(`#(toolSpecification.name=="ToolSearch")`).Exists() {
				t.Fatalf("wrong visible tools: %s", definitions.Raw)
			}
			if loaded.Exists() && loaded.Get("inputSchema.json.properties.value.const").Raw != "9007199254740993" {
				t.Fatal("discovered schema changed")
			}
			result := ctx.Get("toolResults.0")
			if result.Get("toolUseId").String() != tc.resultID || !strings.Contains(result.Get("content.0.text").String(), "ReadLater") || (result.Get("status").String() == "error") != tc.failed {
				t.Fatalf("tool result changed: %s", result.Raw)
			}
		})
	}
}

func TestKiroUnavailableToolChoicesAndInvalidSystemContent(t *testing.T) {
	base := map[string]any{"model": "m", "messages": []any{map[string]string{"role": "user", "content": "go"}}, "tools": []any{map[string]any{"name": "DeferredToolPlaceholder", "defer_loading": true, "input_schema": map[string]string{"type": "object"}}}}
	for _, choice := range []string{"auto", "none", "any", "tool"} {
		base["tool_choice"] = map[string]string{"type": choice, "name": "DeferredToolPlaceholder"}
		body, _ := json.Marshal(base)
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
		_, err := KiroRequest(req, "", "", KiroRuntime{})
		if (err != nil) != (choice == "any" || choice == "tool") {
			t.Errorf("choice=%s, error=%v", choice, err)
		}
		if err == nil {
			raw, _ := io.ReadAll(req.Body)
			if bytes.Contains(raw, []byte("DeferredToolPlaceholder")) {
				t.Fatal("placeholder exposed")
			}
		}
	}
	for _, msgs := range []string{`[{"role":"system","content":[{"type":"tool_use"}]},{"role":"user","content":"hi"}]`, `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":{"wrong":true}}]}]`} {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"m","messages":`+msgs+"}"))
		if _, err := KiroRequest(req, "", "", KiroRuntime{}); err == nil {
			t.Fatal("non-text system message accepted")
		}
	}
}

func TestKiroClaudeCodeInteractiveThinkingDisplay(t *testing.T) {
	const body = `{"model":"m","messages":[{"role":"user","content":"go"}],"thinking":{"type":"adaptive","display":"updates"},"output_config":{"effort":"high"}}`
	for _, tc := range []struct{ allowed, want string }{{`["omitted","summarized"]`, "summarized"}, {`["summarized","updates"]`, "updates"}, {`["omitted"]`, ""}} {
		schema := json.RawMessage(`{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive"]},"display":{"enum":` + tc.allowed + `}}},"output_config":{"properties":{"effort":{"enum":["high"]}}}}}`)
		wire, diagnostics, err := encodeCacheRequest(t, body, KiroRuntime{ModelSchema: schema})
		if tc.want == "" {
			if err == nil {
				t.Fatal("unadvertised display accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		params := wire.Get("additionalModelRequestFields")
		if params.Get("thinking.type").String() != "adaptive" || params.Get("thinking.display").String() != tc.want || params.Get("output_config.effort").String() != "high" {
			t.Fatalf("wrong thinking parameters: %s", params.Raw)
		}
		if tc.want == "summarized" {
			if len(diagnostics) != 1 || diagnostics[0].Code != "kiro_thinking_display_adapted" || diagnostics[0].Path != "thinking.display" {
				t.Fatal(diagnostics)
			}
		} else if len(diagnostics) != 0 {
			t.Fatal(diagnostics)
		}
	}
}
