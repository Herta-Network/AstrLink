package ingress

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/privacy"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/tidwall/gjson"
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
	const secret = "k8Jd2nQ7xR4mT9pL3vZ6"
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, streaming), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/generateAssistantResponse" || r.Header.Get("Authorization") != "Bearer subscription-token" {
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
					content := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.content").String()
					if !strings.Contains(content, "AstrLink") {
						t.Error("caller text was removed")
					}
					payload, _ := json.Marshal(map[string]string{"content": content})
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					_, _ = w.Write(kiroWireFrame("assistantResponseEvent", payload))
					_, _ = w.Write(kiroWireFrame("meteringEvent", []byte(`{"inputTokens":7,"outputTokens":9,"cacheReadInputTokens":2,"cacheWriteInputTokens":1}`)))
				}))
				defer upstream.Close()
				service := contract.Service{ID: "service_kiro", Name: "Kiro", Kind: contract.ServiceKindKiroSubscription, Enabled: true, Models: []string{"claude-sonnet-4.6"}, Capabilities: contract.SubscriptionProviderKiro.Capabilities(), Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderKiro, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/service_kiro"}}
				if protocol != contract.ProtocolAnthropicMessages {
					service.Capabilities = append(service.Capabilities, contract.Capability{Protocol: protocol, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolAnthropicMessages})
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
				handler := NewWithDependencies(Dependencies{
					RequestRecords: records,
					Resolver:       candidateResolver{candidates: []endpoint.Resolved{{Service: service, BaseURL: upstream.URL, Mode: contract.CapabilityModeAdapter, PlanType: contract.PlanTypeAdapter, UpstreamProtocol: contract.ProtocolAnthropicMessages, UpstreamModel: "claude-sonnet-4.6"}}},
					Authorizer:     endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}), ConversionEngine: relaykitbridge.NewEngine(), PrivacyFilter: testPrivacyEngine(t, policy, nil),
				})
				path := "/v1/messages"
				payload := map[string]any{"model": "claude-sonnet-4.6", "stream": streaming, "max_tokens": 128, "messages": []any{map[string]string{"role": "user", "content": "AstrLink api_key=" + secret}}}
				if protocol == contract.ProtocolOpenAIChat {
					path = "/v1/chat/completions"
				}
				if protocol == contract.ProtocolOpenAIResponses {
					path = "/v1/responses"
					delete(payload, "messages")
					payload["input"] = "AstrLink api_key=" + secret
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
				if len(records.records) == 0 {
					t.Fatal("Kiro request was not recorded")
				}
				record := records.records[len(records.records)-1]
				if record.Plan == nil || record.Plan.Type != contract.PlanTypeAdapter || record.Plan.UpstreamProtocol != contract.ProtocolKiroAssistant || record.Plan.AdapterInputProtocol != contract.ProtocolAnthropicMessages || record.Plan.UsesRelayKit() != (protocol != contract.ProtocolAnthropicMessages) {
					t.Fatalf("Kiro record claims the wrong protocol: %+v", record.Plan)
				}

				if record.Usage == nil || record.Usage.InputTokens != 7 || record.Usage.OutputTokens != 9 || record.Usage.BillingIncomplete || record.Usage.CacheReadTokens == nil || *record.Usage.CacheReadTokens != 2 || record.Usage.CacheWriteTokens == nil || *record.Usage.CacheWriteTokens != 1 {
					t.Fatalf("Kiro usage was not recorded correctly: %+v", record.Usage)
				}
				if streaming && !strings.Contains(res.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatalf("missing SSE content type: %v", res.Header())
				}
			})
		}
	}
}
