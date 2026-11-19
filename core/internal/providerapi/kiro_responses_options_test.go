package providerapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

func TestKiroResponsesWireOptions(t *testing.T) {
	const history = `[{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"text.format and previous_response_id are caller text"}]`
	for _, tc := range []struct{ model, reasoning, expectedType, expectedSummary string }{
		{"gpt-5.6-sol", `{"effort":"high","summary":"auto","context":"all_turns"}`, "", ""},
		{"claude-haiku-5.5", `{"effort":"high","summary":"auto"}`, "adaptive", "summarized"},
		{"claude-opus-5.5", `{"effort":"high","type":"adaptive","summary":"omitted"}`, "adaptive", "omitted"},
	} {
		raw := `{"model":"` + tc.model + `","input":` + history + `,"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],
		"tool_choice":"none","parallel_tool_calls":false,"text":{"format":{"type":"text"},"verbosity":"low"},"include":["reasoning.encrypted_content"],
		"service_tier":"auto","truncation":"disabled","background":false,"temperature":1,"top_p":1,"previous_response_id":null,"store":false,
		"user":"caller","safety_identifier":"test","client_metadata":{"trace":"private telemetry"},"reasoning":` + tc.reasoning + `}`
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw))
		ex, err := KiroRequest(req, "profile", "scope", KiroRuntime{})
		if err != nil {
			t.Fatal(err)
		}
		wire, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		out := ojson.ParseBytes(wire)
		for _, key := range []string{"tools", "tool_choice", "parallel_tool_calls", "text", "include", "service_tier", "truncation", "background", "temperature", "top_p", "previous_response_id", "client_metadata"} {
			if out.Get(key).Exists() {
				t.Errorf("%s sent %s", tc.model, key)
			}
		}
		if out.Get("input").Raw != history || out.Get("reasoning.effort").Str != "high" || out.Get("reasoning.context").Exists() || out.Get("reasoning.type").Str != tc.expectedType || out.Get("reasoning.summary").Str != tc.expectedSummary || out.Get("store").Type != ojson.False || out.Get("user").Str != "caller" || out.Get("safety_identifier").Str != "test" {
			t.Fatalf("bad wire for %s: %s", tc.model, wire)
		}
		notes := ex.Diagnostics(contract.ConversionDiagnosticPhaseRequest)
		found := false
		for _, n := range notes {
			if n.Path == "reasoning.summary" {
				found = true
			}
			if strings.Contains(n.Message, "private telemetry") {
				t.Fatal("telemetry leaked")
			}
		}
		if found != (tc.expectedSummary != "omitted") {
			t.Fatalf("wrong summary diagnostic: %v", notes)
		}
	}
}

func TestKiroResponsesRejectsUnrepresentableRequirements(t *testing.T) {
	for _, tc := range []struct{ name, fields, path string }{
		{"id only", `"previous_response_id":"private-id"`, "previous_response_id"},
		{"id with new turn", `"input":"hi","previous_response_id":"private-id"`, "previous_response_id"},
		{"stored item", `"input":[{"type":"item_reference","id":"private-id"}]`, "input"},
		{"stored conversation", `"conversation":"private-id"`, "conversation"},
		{"persistence", `"store":true`, "store"},
		{"background job", `"background":true`, "background"},
		{"hard output cap", `"max_output_tokens":100`, "max_output_tokens"},
		{"custom temperature", `"temperature":0`, "temperature"},
		{"custom top p", `"top_p":0.1`, "top_p"},
		{"required tool", `"tool_choice":"required"`, "tool_choice"},
		{"named tool", `"tool_choice":{"type":"function","name":"private-id"}`, "tool_choice"},
		{"serial tools", `"parallel_tool_calls":false,"tools":[{"type":"function","name":"read"}]`, "parallel_tool_calls"},
		{"schema", `"text":{"format":{"type":"json_schema","strict":true}}`, "text.format"},
		{"extra include", `"include":["web_search_call.results"]`, "include"},
		{"paid tier", `"service_tier":"priority"`, "service_tier"},
		{"auto truncation", `"truncation":"auto"`, "truncation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m",`+tc.fields+`}`))
			_, err := KiroRequest(req, "", "", KiroRuntime{})
			var p *KiroParameterError
			if !errors.As(err, &p) || p.Field != tc.path || strings.Contains(p.Error(), "private-id") {
				t.Fatalf("wrong rejection %v", err)
			}
		})
	}
}

func TestKiroGARThinkingCompatibility(t *testing.T) {
	const adaptive = `{"properties":{"thinking":{"properties":{"type":{"enum":["adaptive","between_tools"]},"display":{"enum":["summarized"]}}},"output_config":{"properties":{"effort":{"enum":["high"]}}}}}`
	const exact = `{"properties":{"thinking":{"properties":{"type":{"enum":["enabled","disabled"]},"budget_tokens":{"type":"integer","minimum":1024}}},"output_config":{"properties":{"effort":{"enum":["high"]}}}}}`
	for _, tc := range []struct {
		name, schema, input, mode, budget, bad string
		notes                                  int
	}{
		{"classic to adaptive", adaptive, `{"type":"enabled","budget_tokens":2048}`, "adaptive", "", "", 2},
		{"native budget", exact, `{"type":"enabled","budget_tokens":2048}`, "enabled", "2048", "", 0},
		{"disable when supported", exact, `{"type":"disabled"}`, "disabled", "", "", 0},
		{"disable otherwise", adaptive, `{"type":"disabled"}`, "", "", "", 1},
		{"between tools rejected by backend", adaptive, `{"type":"between_tools"}`, "", "", "thinking.type", 0},
		{"invalid legacy budget", adaptive, `{"type":"enabled","budget_tokens":0}`, "", "", "thinking.budget_tokens", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, notes, err := encodeCacheRequest(t, `{"model":"claude-sonnet-5.5","messages":[{"role":"user","content":"go"}],"thinking":`+tc.input+`,"output_config":{"effort":"high"}}`, KiroRuntime{ModelSchema: json.RawMessage(tc.schema)})
			if tc.bad != "" {
				var p *KiroParameterError
				if !errors.As(err, &p) || p.Field != tc.bad {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			fields := wire.Get("additionalModelRequestFields")
			if fields.Get("thinking").Exists() != (tc.mode != "") || fields.Get("thinking.type").Str != tc.mode || fields.Get("thinking.budget_tokens").Raw != tc.budget || fields.Get("output_config.effort").Str != "high" || len(notes) != tc.notes {
				t.Fatalf("fields %s, diagnostics %+v", fields.Raw, notes)
			}
		})
	}
}

func TestKiroGARModelCeilingWithoutPerRequestMaxTokens(t *testing.T) {
	rt := KiroRuntime{MaxOutputTokens: 8192}
	for _, tc := range []struct {
		requested string
		accept    bool
	}{{"4096", false}, {"8192", true}, {"64000", true}} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":` + tc.requested + "}"
		wire, _, err := encodeCacheRequest(t, body, rt)
		if tc.accept {
			if err != nil || wire.Get("additionalModelRequestFields.max_tokens").Exists() {
				t.Fatalf("native ceiling %s: %v", tc.requested, err)
			}
		} else {
			var p *KiroParameterError
			if !errors.As(err, &p) || p.Field != "max_tokens" {
				t.Fatalf("smaller cap not protected: %v", err)
			}
		}
	}
}
