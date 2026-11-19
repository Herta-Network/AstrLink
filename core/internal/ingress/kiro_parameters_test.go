package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// Exercise endpoint selection and response affinity as well as both final encoders.
func TestKiroParameterCompatibilityOnPublicRoutes(t *testing.T) {
	service := kiroSettingsCandidate("service_kiro", "").Service
	service.Models = []string{"claude-sonnet-5.5", "claude-haiku-4.5", "gpt-5.6-sol"}
	service.Capabilities = append(service.Capabilities, contract.Capability{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolOpenAIResponses})
	service.ModelProtocols = []contract.ModelProtocol{{Model: "claude-sonnet-5.5", Protocol: contract.ProtocolAnthropicMessages}}
	resolver, err := endpoint.NewStoreResolver(redirectServices{service})
	if err != nil {
		t.Fatal(err)
	}
	engine := relaykitbridge.NewEngine()
	resolver.WithRuntimeProfile(contract.RuntimeProfile{RelayKitAvailable: true, Edges: engine.Edges()})
	var sent []ojson.Result
	handler := NewWithDependencies(Dependencies{
		Resolver: resolver, Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}), ConversionEngine: engine,
		ResolveKiro: func(_ context.Context, _ http.Header, model string, _ contract.ProtocolID) (providerapi.KiroRuntime, error) {
			if model != "claude-sonnet-5.5" {
				return providerapi.KiroRuntime{}, nil
			}
			return providerapi.KiroRuntime{ModelSchema: json.RawMessage(`{"properties":{"max_tokens":{"type":"integer","minimum":1024,"maximum":64000},"thinking":{"properties":{"type":{"enum":["adaptive"]}}}}}`)}, nil
		},
		Forwarder: transport.New(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			sent = append(sent, ojson.ParseBytes(raw))
			ct := "application/vnd.amazon.eventstream"
			body := string(kiroOKFrames())
			if req.URL.Path == "/v1/responses" {
				ct = "text/event-stream"
				body = "data: " + `{"type":"response.completed","response":{"id":"resp_kiro_check","object":"response","status":"completed","output":[]}}` + "\n\n"
			} else if req.URL.Path != "/" {
				t.Errorf("unexpected route %s", req.URL.Path)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {ct}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})),
	})
	call := func(path, body string, code int, field string) ojson.Result {
		t.Helper()
		n := len(sent)
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("%s status %d: %s", path, w.Code, w.Body.String())
		}
		if code == 200 {
			if len(sent) != n+1 {
				t.Fatalf("inference calls %d", len(sent)-n)
			}
			return sent[n]
		}
		resp := ojson.ParseBytes(w.Body.Bytes())
		if len(sent) != n || resp.Get("error.code").Str != "kiro_unsupported_parameter" || resp.Get("error.details.0.field").Str != field || !strings.Contains(resp.Get("error.message").Str, field) || resp.Get("error.retryable").Bool() {
			t.Fatalf("calls=%d error=%s", len(sent)-n, resp.Raw)
		}
		return resp
	}
	wire := call("/v1/responses", `{"model":"gpt-5.6-sol","input":"hello","tool_choice":"auto","parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"store":false,"reasoning":{"effort":"high","summary":"auto"}}`, 200, "")
	if wire.Get("tool_choice").Exists() || wire.Get("parallel_tool_calls").Exists() || wire.Get("include").Exists() || wire.Get("text").Exists() || wire.Get("reasoning.summary").Exists() || wire.Get("reasoning.effort").Str != "high" {
		t.Fatal(wire.Raw)
	}
	for _, body := range []string{`{"model":"gpt-5.6-sol","previous_response_id":"resp_kiro_check","input":"continue"}`, `{"model":"gpt-5.6-sol","previous_response_id":"resp_unknown"}`} {
		r := call("/v1/responses", body, 422, "previous_response_id")
		if !strings.Contains(r.Get("error.message").Str, "full conversation") {
			t.Fatal("missing recovery guidance")
		}
	}
	call("/v1/responses", `{"model":"gpt-5.6-sol","input":"go","max_output_tokens":100}`, 422, "max_output_tokens")
	call("/v1/messages", `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"go"}],"stop_sequences":["END"]}`, 422, "stop_sequences")
	const chat = `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}]`
	for _, stop := range []string{`"END"`, `["END"]`} {
		call("/v1/chat/completions", chat+`,"stop":`+stop+"}", 422, "stop")
	}
	for _, limit := range []string{"max_tokens", "max_completion_tokens"} {
		call("/v1/chat/completions", chat+`,"`+limit+`":100}`, 422, "max_output_tokens")
	}
	call("/v1/chat/completions", chat+`,"tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}],"tool_choice":"required"}`, 422, "tool_choice")
	wire = call("/v1/chat/completions", chat+"}", 200, "")
	if wire.Get("input.0.content").Str != "hi" || wire.Get("model").Str != "claude-sonnet-5.5" {
		t.Fatal(wire.Raw)
	}
	r := call("/v1/messages", `{"model":"claude-sonnet-5-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, 422, "max_tokens")
	if !strings.Contains(r.Get("error.message").Str, "1024") {
		t.Fatal("missing model minimum")
	}
	const history = `,"tool_choice":{"type":"none"},"tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","name":"read","id":"c1","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"result"}]}]}`
	wire = call("/v1/messages", `{"model":"claude-sonnet-5-5","max_tokens":128000,"thinking":{"type":"enabled","budget_tokens":2048}`+history, 200, "")
	u := wire.Get("conversationState.currentMessage.userInputMessage.userInputMessageContext")
	if u.Get("tools").Exists() || u.Get("toolResults.0.toolUseId").Str != "c1" || wire.Get("conversationState.history.1.assistantResponseMessage.toolUses.0.toolUseId").Str != "c1" || wire.Get("additionalModelRequestFields.max_tokens").Int() != 64000 || wire.Get("additionalModelRequestFields.thinking.type").Str != "adaptive" {
		t.Fatal(wire.Raw)
	}
	call("/v1/messages", `{"model":"claude-haiku-4.5"`+history, 422, "tools")
}

func TestKiroConversionPreservesCallerOutputLimit(t *testing.T) {
	for _, tc := range []struct {
		protocol     contract.ProtocolID
		source, want string
	}{
		{contract.ProtocolAnthropicMessages, `{"max_tokens":100}`, "100"},
		{contract.ProtocolOpenAICompletions, `{"messages":[]}`, ""},
		{contract.ProtocolOpenAICompletions, `{"max_tokens":12}`, "12"},
		{contract.ProtocolOpenAICompletions, `{"max_tokens":20,"max_completion_tokens":9}`, "9"},
		{contract.ProtocolGoogleGenerateContent, `{"generationConfig":{"maxOutputTokens":8}}`, "8"},
	} {
		body, err := preserveKiroOutputLimit(tc.protocol, []byte(tc.source), []byte(`{"max_tokens":4096,"model":"m"}`))
		if err != nil || ojson.GetBytes(body, "max_tokens").Raw != tc.want || ojson.GetBytes(body, "model").Str != "m" {
			t.Fatalf("body=%s err=%v", body, err)
		}
	}
}

func TestGatewayThinkingRewriteCannotRaiseKiroOutputLimit(t *testing.T) {
	h := NewWithDependencies(Dependencies{})
	rewrite := gatewayRewriteFrom(reasoningSettings(contract.ReasoningModeFixed, contract.ReasoningLevelHigh))
	for _, tc := range []struct {
		kind contract.ServiceKind
		keep bool
	}{{contract.ServiceKindKiroSubscription, true}, {contract.ServiceKindClaudeSubscription, false}} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
		_, err := h.applyGatewayRewrite(r, contract.ProtocolAnthropicMessages, tc.kind, rewrite, false)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		cap := ojson.GetBytes(b, "max_tokens").Int()
		if (cap == 100) != tc.keep || cap < 100 {
			t.Errorf("kind=%s output cap=%d, body=%s", tc.kind, cap, b)
		}
	}
}
