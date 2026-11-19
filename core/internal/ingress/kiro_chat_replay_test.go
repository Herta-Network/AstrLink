package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

type kiroTestGrant struct{ tokens accountauth.AccountTokens }

func (g *kiroTestGrant) AccessToken(context.Context, contract.ServiceID) (accountauth.AccountTokens, error) {
	return g.tokens, nil
}

func TestKiroChatReplaysReasoningOnToolRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const model = "claude-sonnet-5.5"
			const envelope = "upstream-signed-reasoning"
			grant := &kiroTestGrant{accountauth.AccountTokens{AccessToken: "access-one", RefreshToken: "refresh-one"}}
			candidate := kiroSettingsCandidate("service_kiro", "")
			candidate.Service.Models = []string{model}
			candidate.Service.Capabilities = append(candidate.Service.Capabilities, contract.Capability{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeAdapter, ConvertTo: contract.ProtocolOpenAIResponses, Streaming: true})
			resolver, err := endpoint.NewStoreResolver(redirectServices{candidate.Service})
			if err != nil {
				t.Fatal(err)
			}
			engine := relaykitbridge.NewEngine()
			resolver.WithRuntimeProfile(contract.RuntimeProfile{RelayKitAvailable: true, Edges: engine.Edges()})
			var sent []ojson.Result
			handler := NewWithDependencies(Dependencies{
				Resolver: resolver, ConversionEngine: engine, Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, grant),
				ResolveKiro: func(context.Context, http.Header, string, contract.ProtocolID) (providerapi.KiroRuntime, error) {
					return providerapi.KiroRuntime{Profile: "profile-one", SystemField: stream}, nil
				},
				Forwarder: transport.New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer "+grant.tokens.AccessToken || r.Header.Get(providerapi.KiroGrantHeader) != "" {
						t.Fatal("wrong destination or grant headers")
					}
					sent = append(sent, ojson.ParseBytes(raw))
					var events strings.Builder
					if len(sent) == 1 {
						for i, item := range []string{
							`{"id":"rs1","type":"reasoning","summary":[],"encrypted_content":"` + envelope + `"}`,
							`{"id":"fc1","type":"function_call","call_id":"call1","name":"read","arguments":"{}"}`,
							`{"id":"fc2","type":"function_call","call_id":"call2","name":"read","arguments":"{}"}`,
						} {
							fmt.Fprintf(&events, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, item)
						}
					}
					events.WriteString("data: " + `{"type":"response.completed","response":{"id":"response1","status":"completed","output":[]}}` + "\n\n")
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events.String()))}, nil
				})),
			})
			messages := []any{
				map[string]string{"role": "system", "content": "initial instruction"},
				map[string]string{"role": "user", "content": "first question"},
				map[string]string{"role": "assistant", "content": "first answer"},
				map[string]string{"role": "developer", "content": "later instruction"},
				map[string]string{"role": "user", "content": "read files"},
			}
			send := func(streaming bool) string {
				t.Helper()
				raw, err := json.Marshal(map[string]any{
					"model": model, "messages": messages, "stream": streaming,
					"tools": json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]`),
				})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if w.Code != 200 {
					t.Fatalf("%d %s", w.Code, w.Body.String())
				}
				return w.Body.String()
			}
			first := send(stream)
			if len(sent) != 1 {
				t.Fatal("first request did not reach Kiro")
			}
			wire := sent[0]
			prefix := 0
			if stream {
				if wire.Get("instructions").Str != "initial instruction" {
					t.Fatal(wire.Raw)
				}
			} else {
				prefix = 2
				if wire.Get("instructions").Exists() || wire.Get("input.0.content").Str != "initial instruction" {
					t.Fatal(wire.Raw)
				}
			}
			input := wire.Get("input").Array()
			if len(input) != prefix+4 || input[prefix+1].Get("role").Str != "assistant" || input[prefix+2].Get("role").Str != "user" || input[prefix+2].Get("content").Str != "<system-reminder>\nlater instruction\n</system-reminder>" || input[prefix+3].Get("content").Str != "read files" {
				t.Fatalf("mid-conversation instruction moved: %s", wire.Raw)
			}
			if strings.Contains(first, envelope) || !strings.Contains(first, "call1") || !strings.Contains(first, "call2") {
				t.Fatalf("unexpected Chat tool response: %s", first)
			}
			calls := []any{}
			for _, id := range []string{"call1", "call2"} {
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]string{"name": "read", "arguments": "{}"}})
			}
			if stream {
				messages = append(messages, map[string]any{"role": "assistant", "tool_calls": calls})
			} else {
				messages = append(messages, json.RawMessage(ojson.Get(first, "choices.0.message").Raw))
			}
			for _, id := range []string{"call1", "call2"} {
				messages = append(messages, map[string]string{"role": "tool", "tool_call_id": id, "content": "read successfully"})
			}
			grant.tokens.AccessToken = "access-two"
			send(false)
			second := sent[1].Get("input").Array()
			var order []string
			for _, item := range second {
				switch kind := item.Get("type").Str; kind {
				case "reasoning":
					order = append(order, item.Get("encrypted_content").Str)
				case "function_call", "function_call_output":
					order = append(order, kind+":"+item.Get("call_id").Str)
				}
			}
			if got := strings.Join(order, ","); got != envelope+",function_call:call1,function_call:call2,function_call_output:call1,function_call_output:call2" {
				t.Fatalf("tool turn lost context: %s", got)
			}
			grant.tokens.RefreshToken = "other-grant"
			send(false)
			if strings.Contains(sent[2].Raw, envelope) {
				t.Fatal("reasoning crossed a new grant")
			}
		})
	}
}

func TestKiroNativeHistorySurvivesAccessTokenRefresh(t *testing.T) {
	const model = "claude-sonnet-5.5"
	for _, path := range []string{"/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var bodies []ojson.Result
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				bodies = append(bodies, ojson.ParseBytes(b))
				if path == "/v1/messages" {
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					for _, frame := range [][]byte{
						kiroWireFrame("reasoningContentEvent", []byte(`{"text":"a thought"}`)),
						kiroWireFrame("reasoningContentEvent", []byte(`{"signature":"issued-signature"}`)),
						kiroOKFrames(),
					} {
						_, _ = w.Write(frame)
					}
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"status":"completed","output":[{"type":"reasoning","summary":[],"encrypted_content":"issued-envelope"}]}}`+"\n\n")
				}
			}))
			defer upstream.Close()
			grant := &kiroTestGrant{accountauth.AccountTokens{AccessToken: "first", RefreshToken: "refresh"}}
			candidate := kiroSettingsCandidate("service_native", upstream.URL)
			candidate.UpstreamModel = model
			handler := NewWithDependencies(Dependencies{
				Resolver:    candidateResolver{candidates: []endpoint.Resolved{candidate}},
				Authorizer:  endpoint.NewServiceAuthorizer(codingPlanCredentials{}, grant),
				ResolveKiro: kiroTestTokenLimit,
			})
			send := func(payload string) ojson.Result {
				t.Helper()
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != 200 {
					t.Fatalf("%d %s", recorder.Code, recorder.Body.String())
				}
				return ojson.ParseBytes(recorder.Body.Bytes())
			}
			const user = `{"role":"user","content":"start"}`
			var payload, wirePath, want string
			if path == "/v1/messages" {
				history := send(`{"model":"` + model + `","messages":[` + user + `]}`).Get("content")
				payload = `{"model":"` + model + `","messages":[` + user + `,{"role":"assistant","content":` + history.Raw + `},{"role":"user","content":"continue"}]}`
				wirePath, want = "conversationState.history.1.assistantResponseMessage.reasoningContent.reasoningText.signature", "issued-signature"
			} else {
				history := send(`{"model":"` + model + `","input":[` + user + `]}`).Get("output.0")
				payload = `{"model":"` + model + `","input":[` + user + `,` + history.Raw + `,{"role":"user","content":"continue"}]}`
				wirePath, want = "input.1.encrypted_content", "issued-envelope"
			}
			grant.tokens.AccessToken = "renewed"
			send(payload)
			if len(bodies) != 2 || bodies[1].Get(wirePath).Str != want {
				t.Fatalf("signed history lost on rotation: %v", bodies)
			}
		})
	}
}
