package ingress

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/privacy"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
)

func kiroWireFrame(event string, body []byte) []byte { return kiroWireTypedFrame("event", event, body) }

func kiroWireTypedFrame(messageType, event string, body []byte) []byte {
	var headers bytes.Buffer
	for _, p := range [][2]string{{":message-type", messageType}, {":event-type", event}} {
		headers.WriteByte(byte(len(p[0])))
		headers.WriteString(p[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(p[1])))
		headers.WriteString(p[1])
	}
	frame := make([]byte, 12)
	binary.BigEndian.PutUint32(frame[:4], uint32(16+headers.Len()+len(body)))
	binary.BigEndian.PutUint32(frame[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(frame[8:], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, body...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
}

func TestKiroEndToEndMessagesConvertedStreamingAndPrivacy(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses} {
		responses := protocol != contract.ProtocolAnthropicMessages
		for _, streaming := range []bool{false, true} {
			for _, counts := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/responses=%t/stream=%t/counts=%t", protocol, responses, streaming, counts), func(t *testing.T) { testKiroIngressCase(t, protocol, responses, streaming, counts) })
			}
		}
	}
}

func testKiroIngressCase(t *testing.T, protocol contract.ProtocolID, responses, streaming, counts bool) {
	t.Helper()
	const secret = "k8Jd2nQ7xR4mT9pL3vZ6"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/"
		if responses {
			wantPath = "/v1/responses"
		}
		if r.URL.Path != wantPath || r.Header.Get("Authorization") != "Bearer subscription-token" {
			t.Errorf("upstream target/auth: %s %v", r.URL, r.Header)
		}
		for name, values := range r.Header {
			if strings.Contains(strings.ToLower(name+strings.Join(values, "")), "astrlink") {
				t.Errorf("gateway identity escaped: %s", name)
			}
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Error("privacy protection bypassed")
		}
		if bytes.Contains(raw, []byte("local-prefix-key")) {
			t.Error("caller cache key reached the wire")
		}
		wantPoint := protocol == contract.ProtocolAnthropicMessages && !responses
		if got := ojson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.cachePoint.type").String(); (got == "default") != wantPoint {
			t.Errorf("checkpoint=%q wantPoint=%v", got, wantPoint)
		}
		content := ojson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.content").String()
		if responses {
			items := ojson.GetBytes(raw, "input").Array()
			if len(items) > 0 {
				content = items[len(items)-1].Get("content").String()
			}
		}
		if !strings.Contains(content, "AstrLink") {
			t.Error("caller text was removed")
		}
		if responses {
			w.Header().Set("Content-Type", "text/event-stream")
			payload, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": content})
			fmt.Fprintf(w, "data: %s\n\n", payload)
			usage := `{"kiro:credits":{"value":0.011,"unit":"credit","unit_plural":"credits"}}`
			if counts {
				usage = `{"input_tokens":7,"output_tokens":9,"total_tokens":16,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}`
			}
			item, _ := json.Marshal(map[string]any{"type": "message", "content": []any{map[string]string{"type": "output_text", "text": content}}})
			fmt.Fprintf(w, "data: "+`{"type":"response.completed","response":{"id":"resp_kiro","status":"completed","output":[%s],"usage":%s}}`+"\n\n", item, usage)
			return
		}
		payload, _ := json.Marshal(map[string]string{"content": content})
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroWireFrame("assistantResponseEvent", payload))
		if counts {
			_, _ = w.Write(kiroWireFrame("metadataEvent", []byte(`{"stopReason":"END_TURN","tokenUsage":{"uncachedInputTokens":4,"outputTokens":9,"totalTokens":16,"cacheReadInputTokens":2,"cacheWriteInputTokens":1}}`)))
		}
		_, _ = w.Write(kiroWireFrame("meteringEvent", []byte(`{"usage":0.011,"unit":"credit","unitPlural":"credits"}`)))
	}))
	defer upstream.Close()
	service := contract.Service{ID: "service_kiro", Name: "Kiro", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"claude-sonnet-4.6"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/service_kiro"}}
	if protocol == contract.ProtocolOpenAIChat {
		service.Capabilities = append(service.Capabilities, contract.Capability{Protocol: protocol, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolOpenAIResponses})
	}
	if err := service.Validate(); err != nil {
		t.Fatal(err)
	}
	policy, err := privacy.FromContractPolicy(contract.DefaultPrivacyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy.ResponseRestore = true
	records := &memoryRequestRecordStore{}
	var engine relaykitbridge.ConversionEngine
	if protocol == contract.ProtocolOpenAIChat {
		engine = relaykitbridge.NewEngine()
	}
	handler := NewWithDependencies(Dependencies{
		ResolveKiro: func(_ context.Context, h http.Header, model string, format contract.ProtocolID) (providerapi.KiroRuntime, error) {
			if (format == contract.ProtocolOpenAIResponses) != responses || model != "claude-sonnet-4.6" || h.Get("Authorization") != "Bearer subscription-token" {
				t.Fatal("Kiro metadata looked up outside the selected account or model")
			}
			return providerapi.KiroRuntime{ModelSchema: json.RawMessage(`{"properties":{"max_tokens":{"type":"integer"}}}`), PromptCaching: providerapi.KiroPromptCaching{SupportsPromptCaching: true}}, nil
		},
		RequestRecords: records,
		Resolver:       candidateResolver{candidates: []endpoint.Resolved{{Service: service, BaseURL: upstream.URL, Mode: contract.CapabilityModeAdapter, PlanType: contract.PlanTypeAdapter, UpstreamProtocol: contract.ProtocolAnthropicMessages, UpstreamModel: "claude-sonnet-4.6"}}},
		Authorizer:     endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}), ConversionEngine: engine, PrivacyFilter: testPrivacyEngine(t, policy, nil),
	})
	path := "/v1/messages"
	payload := map[string]any{"model": "claude-sonnet-4.6", "stream": streaming, "max_tokens": 128, "messages": []any{map[string]string{"role": "user", "content": "AstrLink api_key=" + secret}}}
	if protocol == contract.ProtocolOpenAIChat {
		path = "/v1/chat/completions"
		delete(payload, "max_tokens")
	}
	if protocol == contract.ProtocolOpenAIResponses {
		path = "/v1/responses"
		delete(payload, "messages")
		delete(payload, "max_tokens")
		payload["input"] = "AstrLink api_key=" + secret
	}
	if protocol == contract.ProtocolAnthropicMessages {
		payload["cache_control"] = map[string]string{"type": "ephemeral"}
	} else {
		payload["prompt_cache_key"] = "local-prefix-key"
		payload["prompt_cache_retention"] = "24h"
	}
	raw, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local-token")
	req.Header.Set("User-Agent", "AstrLink-test-client")
	req.Header.Set("X-AstrLink-Injected", "private")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 200 || !strings.Contains(res.Body.String(), secret) {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
	if protocol == contract.ProtocolOpenAIChat {
		if streaming {
			if !strings.HasSuffix(strings.TrimSpace(res.Body.String()), "data: [DONE]") || !strings.Contains(res.Body.String(), `"object":"chat.completion.chunk"`) {
				t.Fatalf("invalid Chat event stream: %s", res.Body.String())
			}
		} else if ojson.GetBytes(res.Body.Bytes(), "object").Str != "chat.completion" || !strings.Contains(ojson.GetBytes(res.Body.Bytes(), "choices.0.message.content").Str, secret) {
			t.Fatalf("invalid Chat completion: %s", res.Body.String())
		}
	}
	if len(records.records) == 0 {
		t.Fatal("Kiro request was not recorded")
	}
	record := records.records[len(records.records)-1]
	wire, input := contract.ProtocolKiroAssistant, contract.ProtocolAnthropicMessages
	if responses {
		wire, input = contract.ProtocolKiroResponses, contract.ProtocolOpenAIResponses
	}
	if record.Plan == nil || record.Plan.Type != contract.PlanTypeAdapter || record.Plan.UpstreamProtocol != wire || record.Plan.AdapterInputProtocol != input || record.Plan.UsesRelayKit() != (protocol == contract.ProtocolOpenAIChat) {
		t.Fatalf("Kiro record claims the wrong protocol: %+v", record.Plan)
	}

	cacheLosses := map[string]bool{}
	for _, d := range record.ConversionDiagnostics {
		if d.Code == "kiro_cache_control_omitted" || d.Code == "kiro_cache_key_omitted" {
			cacheLosses[d.Path] = true
		}
	}
	if protocol == contract.ProtocolAnthropicMessages {
		if cacheLosses["cache_control"] != responses {
			t.Fatalf("checkpoint diagnostic: %v", cacheLosses)
		}
	} else if !cacheLosses["prompt_cache_key"] || !cacheLosses["prompt_cache_retention"] {
		t.Fatalf("missing cache loss after conversion: %v", cacheLosses)
	}
	assertKiroClientUsage(t, protocol, streaming, counts, res.Body.Bytes())
	if !counts {
		if record.Usage != nil {
			t.Fatalf("provider did not report token usage, got %+v", record.Usage)
		}
	} else if record.Usage == nil || record.Usage.InputTokens != 7 || record.Usage.OutputTokens != 9 || record.Usage.BillingIncomplete || record.Usage.CacheReadTokens == nil || *record.Usage.CacheReadTokens != 2 || record.Usage.CacheWriteTokens == nil || *record.Usage.CacheWriteTokens != 1 {
		t.Fatalf("Kiro usage was not recorded correctly: %+v", record.Usage)
	}
	if streaming && !strings.Contains(res.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("missing SSE content type: %v", res.Header())
	}
}

func assertKiroClientUsage(t *testing.T, protocol contract.ProtocolID, stream, known bool, body []byte) {
	t.Helper()
	if protocol == contract.ProtocolAnthropicMessages {
		return
	}
	seen := false
	check := func(usage ojson.Result) {
		t.Helper()
		if !usage.IsObject() {
			return
		}
		if protocol == contract.ProtocolOpenAIResponses && !usage.Get("input_tokens").Exists() {
			if known || !usage.Get("kiro:credits").Exists() {
				t.Fatalf("unexpected native usage: %s", usage.Raw)
			}
			return
		}
		seen = true
		if !known {
			t.Fatal("converted client response reported unobserved Kiro usage")
		}
		input, output := "prompt_tokens", "completion_tokens"
		if protocol == contract.ProtocolOpenAIResponses {
			input, output = "input_tokens", "output_tokens"
		}
		for field, want := range map[string]int64{input: 7, output: 9, "total_tokens": 16, input + "_details.cached_tokens": 2, input + "_details.cache_write_tokens": 1} {
			if got := usage.Get(field); got.Type != ojson.Number || got.Int() != want {
				t.Errorf("converted client usage %s: %s", field, usage.Raw)
			}
		}
	}
	if !stream {
		check(ojson.GetBytes(body, "usage"))
	} else {
		for _, line := range strings.Split(string(body), "\n") {
			raw, ok := strings.CutPrefix(line, "data: ")
			if !ok || raw == "[DONE]" {
				continue
			}
			if protocol == contract.ProtocolOpenAIResponses {
				if ojson.Get(raw, "type").String() == "response.created" && ojson.Get(raw, "response.usage").IsObject() {
					t.Fatal("created event reported provisional Kiro usage")
				}
				check(ojson.Get(raw, "response.usage"))
			} else {
				check(ojson.Get(raw, "usage"))
			}
		}
	}
	// Streaming Chat clients can omit the optional usage chunk.
	if known && (!stream || protocol == contract.ProtocolOpenAIResponses) && !seen {
		t.Fatal("client response lost observed Kiro usage")
	}
}
