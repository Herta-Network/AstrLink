package providerapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestKiroRuntimeReasoningRoundTrip(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%t/stream=%t", responses, stream), func(t *testing.T) {
				runtime := KiroRuntime{Responses: responses}
				cache := new(KiroReplay)
				runtime.BindReplay(cache, "service_a", "profile_a", "grant_a", "caller")
				const thought = "signed thought"
				const sig = "from upstream"
				var data []byte
				media := "application/vnd.amazon.eventstream"
				if responses {
					media = "text/event-stream"
					data = []byte(`data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"display summary"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"{\"reasoningText\":{\"text\":\"signed thought\",\"signature\":\"from upstream\"}}"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"reply"}]}}

data: {"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}

`)
				} else {
					data = bytes.Join([][]byte{kiroFrame("reasoningContentEvent", `{"text":"signed thought"}`), kiroFrame("reasoningContentEvent", `{"signature":"from upstream"}`), kiroFrame("assistantResponseEvent", `{"content":"reply"}`), kiroFrame("metadataEvent", `{"stopReason":"END_TURN"}`)}, nil)
				}
				res := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(bytes.NewReader(data))}
				if err := KiroResponse(res, &KiroExchange{Model: "model", Streaming: stream, Responses: responses, replay: runtime.replay}); err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(res.Body)
				res.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), thought) || !strings.Contains(string(raw), sig) || strings.Contains(string(raw), "display summary") {
					t.Fatalf("reasoning changed: %s", raw)
				}
				if stream && !strings.Contains(string(raw), "signature_delta") {
					t.Fatalf("signature missing: %s", raw)
				}
				if responses && !strings.Contains(string(raw), `"cache_read_input_tokens":4`) {
					t.Fatalf("usage missing: %s", raw)
				}
				replay := func(rt KiroRuntime, txt string) (string, error) {
					b, _ := json.Marshal(map[string]any{"model": "model", "messages": []any{map[string]string{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": []map[string]string{{"type": "thinking", "thinking": txt, "signature": sig}, {"type": "text", "text": "reply"}}}, map[string]string{"role": "user", "content": "continue"}}})
					req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(b))
					_, err := KiroRequest(req, "profile_a", "scope", rt)
					if err != nil {
						return "", err
					}
					wire, _ := io.ReadAll(req.Body)
					return string(wire), nil
				}
				wire, err := replay(runtime, thought)
				if err != nil {
					t.Fatal(err)
				}
				reasoning := ojson.Get(wire, "conversationState.history.1.assistantResponseMessage.reasoningContent")
				if responses {
					reasoning = ojson.Parse(ojson.Get(wire, "input.1.encrypted_content").String())
				}
				if reasoning.Get("reasoningText.text").String() != thought || reasoning.Get("reasoningText.signature").String() != sig {
					t.Fatalf("replay changed: %s", wire)
				}
				if _, err := replay(runtime, "modified"); !errors.Is(err, ErrKiroThinkingUnsupported) {
					t.Fatalf("modified reasoning accepted: %v", err)
				}
				runtime.BindReplay(cache, "service_a", "profile_a", "grant_b", "caller")
				if _, err := replay(runtime, thought); !errors.Is(err, ErrKiroThinkingUnsupported) {
					t.Fatalf("another grant accepted reasoning: %v", err)
				}
			})
		}
	}
}

func TestKiroResponsesInterleavedToolsAndTruncatedStream(t *testing.T) {
	events := `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"itemA","type":"function_call","call_id":"callA","name":"read"}}

data: {"type":"response.output_item.added","output_index":1,"item":{"id":"itemB","type":"function_call","call_id":"callB","name":"write"}}

data: {"type":"response.function_call_arguments.delta","item_id":"itemA","delta":"{\"count\":"}

data: {"type":"response.function_call_arguments.done","item_id":"itemB","arguments":"{}"}

data: {"type":"response.output_item.done","item":{"id":"itemA","call_id":"callA","type":"function_call","arguments":"{\"count\":9007199254740993}"}}

`
	for _, tt := range []struct {
		name, tail string
		ok         bool
	}{
		{"complete", `data: {"type":"response.completed","response":{}}` + "\n\n", true},
		{"truncated", "", false},
		{"upstream error", `data: {"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}` + "\n\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				res := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events + tt.tail))}
				err := KiroResponse(res, &KiroExchange{Model: "m", Streaming: stream, Responses: true})
				var body []byte
				if err == nil {
					body, err = io.ReadAll(res.Body)
				}
				res.Body.Close()
				if tt.ok != (err == nil) {
					t.Fatalf("stream=%v err=%v", stream, err)
				}
				if tt.ok && (!bytes.Contains(body, []byte("9007199254740993")) || !bytes.Contains(body, []byte("callA")) || !bytes.Contains(body, []byte("callB"))) {
					t.Fatalf("tool reconstruction: %s", body)
				}
				if !tt.ok && bytes.Contains(body, []byte("message_stop")) {
					t.Fatalf("early success: %s", body)
				}
			}
		})
	}
}

func TestKiroAuthorizedSystemFieldAndModelParameters(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive"]},"display":{"enum":["summarized","omitted"]}}},"output_config":{"properties":{"effort":{"enum":["high","low"]}}},"max_tokens":{"type":"integer","minimum":1024,"maximum":64000}}}`)
	const prompt = "caller authored instructions"
	const body = `{"model":"m","system":"caller authored instructions","messages":[{"role":"user","content":"go"}],"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"},"max_tokens":2000}`
	for _, responses := range []bool{false, true} {
		for _, field := range []bool{false, true} {
			rt := KiroRuntime{Responses: responses, SystemField: field, ModelSchema: schema}
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			ex, err := KiroRequest(req, "profile", "scope", rt)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(req.Body)
			r := ojson.ParseBytes(raw)
			if ex.Responses != responses || bytes.Count(raw, []byte(prompt)) != 1 {
				t.Fatalf("system prompt missing or duplicated: %s", raw)
			}
			if responses {
				if r.Get("instructions").Exists() != field || r.Get("reasoning.effort").String() != "high" || r.Get("reasoning.summary").String() != "summarized" || r.Get("reasoning.type").String() != "adaptive" {
					t.Fatalf("wrong Responses fields: %s", raw)
				}
				if !field && r.Get("input.0.content").String() != prompt {
					t.Fatal("missing historical prompt")
				}
			} else {
				if r.Get("systemPrompt").Exists() != field || r.Get("additionalModelRequestFields.max_tokens").Int() != 2000 || r.Get("additionalModelRequestFields.output_config.effort").String() != "high" {
					t.Fatalf("wrong GAR fields: %s", raw)
				}
				if !field && r.Get("conversationState.history.0.userInputMessage.content").String() != prompt {
					t.Fatal("missing historical prompt")
				}
			}
			req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(strings.Replace(body, `"high"`, `"unknown"`, 1)))
			if _, err := KiroRequest(req, "profile", "scope", rt); err == nil {
				t.Fatal("unsupported effort accepted")
			}
		}
	}
}

func TestKiroRedactedReasoningAcrossEndpoints(t *testing.T) {
	const encoded = "AQIDBA=="
	const model = "unit"
	for _, sourceResponses := range []bool{false, true} {
		runtime := KiroRuntime{}
		runtime.BindReplay(new(KiroReplay), "service", "profile", "account", "principal")
		frames := append(kiroFrame("reasoningContentEvent", `{"redactedContent":"AQIDBA=="}`), kiroFrame("metadataEvent", `{"stopReason":"END_TURN"}`)...)
		media := "application/vnd.amazon.eventstream"
		if sourceResponses {
			media = "text/event-stream"
			frames = []byte(`data: {"type":"response.completed","response":{"output":[{"type":"reasoning","encrypted_content":"{\"redactedContent\":\"AQIDBA==\"}"}]}}

`)
		}
		res := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(bytes.NewReader(frames))}
		if err := KiroResponse(res, &KiroExchange{Model: model, Responses: sourceResponses, replay: runtime.replay}); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		block := ojson.GetBytes(raw, "content.0")
		if block.Get("data").String() != encoded || block.Get("type").String() != "redacted_thinking" {
			t.Fatalf("bad redacted block: %s", block.Raw)
		}
		history := `{"model":"unit","messages":[{"role":"user","content":"go"},{"role":"assistant","content":[` + block.Raw + `]},{"role":"user","content":"again"}]}`
		runtime.Responses = !sourceResponses
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(history))
		if _, err = KiroRequest(req, "profile", "scope", runtime); err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(req.Body)
		envelope := ojson.GetBytes(out, "conversationState.history.1.assistantResponseMessage.reasoningContent")
		if runtime.Responses {
			envelope = ojson.Parse(ojson.GetBytes(out, "input.1.encrypted_content").String())
		}
		if envelope.Get("redactedContent").String() != encoded {
			t.Fatalf("lost redacted content on replay: %s", out)
		}
	}
}

func TestKiroResponsesCarriesToolsAndAttachments(t *testing.T) {
	const body = `{
 "model":"model","tools":[{"name":"read","description":"caller tool","input_schema":{"type":"object"}}],
 "messages":[
  {"role":"user","content":"read it"},
  {"role":"assistant","content":[{"type":"tool_use","id":"callA","name":"read","input":{"seq":9007199254740993}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"callA","content":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1n"}},{"type":"document","title":"report","source":{"type":"text","media_type":"text/plain","data":"hello"}}]}
 ]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	if _, err := KiroRequest(req, "profile", "scope", KiroRuntime{Responses: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(req.Body)
	r := ojson.ParseBytes(raw)
	want := map[string]string{
		"input.1.type": "function_call", "input.1.call_id": "callA", "input.1.arguments": `{"seq":9007199254740993}`,
		"input.2.type": "function_call_output", "input.2.call_id": "callA", "input.2.output": "ok",
		"tools.0.name": "read", "tools.0.description": "caller tool", "tools.0.parameters.type": "object",
	}
	for path, value := range want {
		if got := r.Get(path).String(); got != value {
			t.Errorf("%s=%q, want %q", path, got, value)
		}
	}
	parts := r.Get("input.3.content")
	if parts.Get(`#(type=="input_image").image_url`).String() != "data:image/png;base64,aW1n" ||
		parts.Get(`#(type=="input_file").file_data`).String() != "data:text/plain;base64,aGVsbG8=" {
		t.Fatalf("attachments changed: %s", parts.Raw)
	}
}
