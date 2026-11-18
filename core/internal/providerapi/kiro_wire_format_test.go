package providerapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestKiroFinalRequestsMatchRuntimeOperations(t *testing.T) {
	const profile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/unit"
	const messages = `{"model":"model","messages":[{"role":"user","content":"hello"}]}`
	for _, responses := range []bool{false, true} {
		req := httptest.NewRequest("POST", DefaultKiroRuntimeURL+"/v1/messages?ignored=1", strings.NewReader(messages))
		identity := KiroIdentityAt("0.14.0", true)
		ApplyKiroHeaders(req.Header, "allowed-token", identity)
		req.Header.Set("TokenType", "EXTERNAL_IDP")
		req.Header.Set("X-Amzn-Kiro-Attempt", "99;max=99")
		req.Header.Set("X-AstrLink-Caller-Header", "must not leave")
		req.Header.Set("X-Amzn-Kiro-Profile", "foreign")
		req.Header.Set("X-Amzn-Kiro-Profile-Arn", "foreign")

		if _, err := KiroRequest(req, profile, "", KiroRuntime{Responses: responses}); err != nil {
			t.Fatal(err)
		}
		check := func(key, want string) {
			t.Helper()
			if got := req.Header.Get(key); got != want {
				t.Errorf("responses=%v %s=%q, want %q", responses, key, got, want)
			}
		}
		check("User-Agent", identity.UserAgent)
		check("Authorization", "Bearer allowed-token")
		check("X-Amzn-Kiro-Attempt", "1;max=1")
		check("X-Amzn-Codewhisperer-Optout", "true")
		check("X-AstrLink-Caller-Header", "")
		check("TokenType", "")
		check("X-Amzn-Kiro-Profile-Arn", "")
		if req.URL.Host != "runtime.eu-central-1.kiro.dev" || req.URL.RawQuery != "" || req.Host != "" {
			t.Fatalf("incorrect regional destination: %s (Host %q)", req.URL, req.Host)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := ojson.GetBytes(body, "profileArn").String(); got != profile {
			t.Fatalf("wrong profile: %s", got)
		}
		if responses {
			if req.URL.Path != "/v1/responses" {
				t.Fatalf("wrong Responses path: %s", req.URL.Path)
			}
			check("Content-Type", "application/json")
			check("Accept", "text/event-stream")
			check("X-Amzn-Kiro-Origin", "AI_EDITOR")
			check("X-Amzn-Kiro-Profile", profile)
			check("X-Amz-Target", "")
			check("X-Amz-User-Agent", "")
			check("Amz-Sdk-Request", "")
			check("Amz-Sdk-Invocation-Id", "")
			check("X-Amzn-Kiro-Agent-Mode", "")
			continue
		}
		if req.URL.Path != "/" || ojson.GetBytes(body, "agentMode").String() != "vibe" {
			t.Fatalf("wrong GenerateAssistantResponse wire: %s %s", req.URL.Path, body)
		}
		check("Content-Type", "application/x-amz-json-1.0")
		check("Accept", "application/vnd.amazon.eventstream")
		check("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
		check("Amz-Sdk-Request", "attempt=1; max=1")
		check("X-Amz-User-Agent", identity.AmzUserAgent)
		check("X-Amzn-Kiro-Profile", "")
	}
}

func TestKiroToolResultWire(t *testing.T) {
	const body = `{
 "model":"model",
 "tools":[{"name":"read","input_schema":{"type":"object"}}],
 "messages":[
   {"role":"user","content":"go"},
   {"role":"assistant","content":[{"type":"tool_use","id":"call_one","name":"read","input":{}}]},
   {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_one","is_error":true,"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]}
 ]}`
	for _, responses := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		ex, err := KiroRequest(req, "profile", "", KiroRuntime{Responses: responses})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if ex.ToolResultStatusOmitted != responses {
			t.Errorf("responses=%v: omitted error status=%v", responses, ex.ToolResultStatusOmitted)
		}
		if responses {
			item := ojson.GetBytes(wire, "input.2")
			if item.Get("type").String() != "function_call_output" || item.Get("call_id").String() != "call_one" || item.Get("output").String() != "first\nsecond" {
				t.Fatalf("incorrect function output: %s", item.Raw)
			}
			continue
		}
		result := ojson.GetBytes(wire, "conversationState.currentMessage.userInputMessage.userInputMessageContext.toolResults.0")
		if result.Get("status").String() != "error" || result.Get("toolUseId").String() != "call_one" ||
			result.Get("content.0.text").String() != "first" || result.Get("content.1.text").String() != "second" {
			t.Fatalf("incorrect GAR tool result: %s", result.Raw)
		}
	}
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(strings.Replace(body, `"tools":[{"name":"read","input_schema":{"type":"object"}}],`, "", 1)))
	if _, err := KiroRequest(req, "profile", "", KiroRuntime{}); err == nil {
		t.Fatal("GAR accepted tool history without a declared tool")
	}
}

func TestKiroRefusesUnrepresentableReasoningOptions(t *testing.T) {
	const schema = `{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive","disabled"]}}}}}`
	for _, option := range []string{
		`"thinking":{"type":"adaptive","unsupported":true}`,
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}`,
		`"output_config":"high"`,
	} {
		for _, responses := range []bool{false, true} {
			body := []byte(`{"model":"model","messages":[{"role":"user","content":"go"}],` + option + "}")
			req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
			if _, err := KiroRequest(req, "profile", "", KiroRuntime{Responses: responses, ModelSchema: json.RawMessage(schema)}); err == nil {
				t.Errorf("responses=%v: accepted unsupported option %s", responses, option)
			}
		}
	}
}

func TestKiroRejectsToolRequirementsMissingFromBothRuntimeFormats(t *testing.T) {
	for _, tc := range []struct {
		name, toolExtra, choiceExtra string
		reject                       bool
	}{
		{name: "ordinary", toolExtra: `,"strict":false,"defer_loading":false`, choiceExtra: `,"disable_parallel_tool_use":false`},
		{name: "serial", choiceExtra: `,"disable_parallel_tool_use":true`, reject: true},
		{name: "strict", toolExtra: `,"strict":true`, reject: true},
		{name: "deferred", toolExtra: `,"defer_loading":true`, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"use a tool"}],` +
				`"tools":[{"name":"read","input_schema":{"type":"object"}` + tc.toolExtra + `}],` +
				`"tool_choice":{"type":"auto"` + tc.choiceExtra + `}}`
			for _, responses := range []bool{false, true} {
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				_, err := KiroRequest(req, "profile", "", KiroRuntime{Responses: responses})
				if (err != nil) != tc.reject {
					t.Errorf("responses=%t got error %v; reject=%t", responses, err, tc.reject)
				}
			}
		})
	}
}
