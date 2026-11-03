package ingress

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/privacy"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/tidwall/gjson"
)

func TestKiroDocumentsAndForcedToolsAcrossProtocols(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses} {
		for _, format := range []struct{ ext, media string }{{"pdf", "application/pdf"}, {"csv", "text/csv"}, {"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"}} {
			t.Run(string(protocol)+"/"+format.ext, func(t *testing.T) {
				data := base64.StdEncoding.EncodeToString([]byte("document bytes remain intact"))
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					raw, _ := io.ReadAll(r.Body)
					current := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage")
					docs := current.Get("documents").Array()
					if len(docs) != 1 || docs[0].Get("format").String() != format.ext || docs[0].Get("source.bytes").String() != data {
						t.Errorf("document lost in %s: %s", protocol, raw)
					}
					tools := current.Get("userInputMessageContext.tools").Array()
					if len(tools) != 1 || tools[0].Get("toolSpecification.name").String() != "read" || !strings.Contains(current.Get("content").String(), `requires the tool "read"`) {
						t.Errorf("forced tool compatibility lost: %s", raw)
					}
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					_, _ = w.Write(kiroOKFrames())
				}))
				defer upstream.Close()
				candidate := kiroSettingsCandidate("service_documents", upstream.URL)
				if protocol != contract.ProtocolAnthropicMessages {
					candidate.Service.Capabilities = append(candidate.Service.Capabilities, contract.Capability{Protocol: protocol, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolAnthropicMessages})
				}
				handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{candidate}}, ConversionEngine: relaykitbridge.NewEngine(), Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{})})
				payload := map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 100}
				schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
				path := "/v1/messages"
				switch protocol {
				case contract.ProtocolAnthropicMessages:
					payload["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document", "title": "file." + format.ext, "source": map[string]any{"type": "base64", "media_type": format.media, "data": data}}}}}
					payload["tools"] = []any{map[string]any{"name": "read", "input_schema": schema}, map[string]any{"name": "ignored", "input_schema": schema}}
					payload["tool_choice"] = map[string]string{"type": "tool", "name": "read"}
				case contract.ProtocolOpenAIChat:
					path = "/v1/chat/completions"
					payload["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "file", "file": map[string]string{"filename": "file." + format.ext, "file_data": "data:" + format.media + ";base64," + data}}}}}
					payload["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": schema}}, map[string]any{"type": "function", "function": map[string]any{"name": "ignored", "parameters": schema}}}
					payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": "read"}}
				case contract.ProtocolOpenAIResponses:
					path = "/v1/responses"
					payload["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_file", "filename": "file." + format.ext, "file_data": "data:" + format.media + ";base64," + data}}}}
					payload["tools"] = []any{map[string]any{"type": "function", "name": "read", "parameters": schema}, map[string]any{"type": "function", "name": "ignored", "parameters": schema}}
					payload["tool_choice"] = map[string]string{"type": "function", "name": "read"}
				}
				raw, _ := json.Marshal(payload)
				req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != 200 || calls != 1 {
					t.Fatalf("status=%d calls=%d body=%s", res.Code, calls, res.Body.String())
				}
			})
		}
	}
}

func TestKiroPlaintextDocumentsAreInspectedBeforeEncoding(t *testing.T) {
	const secret = "k8Jd2nQ7xR4mT9pL3vZ6"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		data := gjson.GetBytes(raw, "conversationState.currentMessage.userInputMessage.documents.0.source.bytes").String()
		decoded, _ := base64.StdEncoding.DecodeString(data)
		if strings.Contains(string(decoded), secret) || !strings.Contains(string(decoded), "api_key=") {
			t.Errorf("text document privacy failed: %s", decoded)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroOKFrames())
	}))
	defer upstream.Close()
	policy, err := privacy.FromContractPolicy(contract.DefaultPrivacyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{kiroSettingsCandidate("service_text", upstream.URL)}}, PrivacyFilter: testPrivacyEngine(t, policy, nil), Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{})})
	body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"document","title":"config.txt","source":{"type":"text","data":"api_key=` + secret + `"}}]}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("%d %s", res.Code, res.Body.String())
	}
}

func TestKiroRequiredToolChoiceThroughOpenAIConversion(t *testing.T) {
	for _, protocol := range []contract.ProtocolID{contract.ProtocolOpenAIChat, contract.ProtocolOpenAIResponses} {
		body := map[string]any{"model": "m", "tool_choice": "required", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]string{"type": "object"}}}}, "messages": []any{map[string]string{"role": "user", "content": "hello"}}}
		if protocol == contract.ProtocolOpenAIResponses {
			delete(body, "messages")
			body["input"] = "hello"
			body["tools"] = []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]string{"type": "object"}}}
		}
		raw, _ := json.Marshal(body)
		converted, err := relaykitbridge.NewEngine().ConvertRequest(t.Context(), relaykitbridge.ConvertRequestInput{From: protocol, To: contract.ProtocolAnthropicMessages, ContentType: "application/json", Body: raw, PublicModel: "m", UpstreamModel: "m"})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(string(converted.Body)))
		if _, err := providerapi.KiroRequest(req, "", ""); err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(req.Body)
		if !strings.Contains(gjson.GetBytes(out, "conversationState.currentMessage.userInputMessage.content").String(), "Call at least one available tool") {
			t.Fatalf("%s dropped required tool choice: %s", protocol, out)
		}
	}
}

func TestKiroResponsesToolOutputDocumentPreservesCallAssociation(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("spreadsheet bytes"))
	body := `{"model":"m","input":[{"role":"user","content":"read"},{"type":"function_call","call_id":"call_one","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_one","output":[{"type":"input_text","text":"tool result"},{"type":"input_file","filename":"report.xlsx","file_data":"data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64,` + data + `"}]}]}`
	prepared, documents, err := providerapi.PrepareKiroDocuments(contract.ProtocolOpenAIResponses, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := relaykitbridge.NewEngine().ConvertRequest(t.Context(), relaykitbridge.ConvertRequestInput{From: contract.ProtocolOpenAIResponses, To: contract.ProtocolAnthropicMessages, ContentType: "application/json", Body: prepared, PublicModel: "m", UpstreamModel: "m"})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := documents.Restore(converted.Body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(string(restored)))
	if _, err := providerapi.KiroRequest(req, "", ""); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(req.Body)
	current := gjson.GetBytes(out, "conversationState.currentMessage.userInputMessage")
	if current.Get("documents.0.source.bytes").String() != data || current.Get("documents.0.format").String() != "xlsx" || current.Get("userInputMessageContext.toolResults.0.toolUseId").String() != "call_one" || !strings.Contains(current.Get("userInputMessageContext.toolResults.0.content.0.text").String(), "tool result") {
		t.Fatalf("tool document/result lost: %s", out)
	}
}

func TestKiroChatToolOutputDocumentPreservesCallAssociation(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("document bytes"))
	body := `{"model":"m","messages":[{"role":"user","content":"read"},{"role":"assistant","tool_calls":[{"id":"call_one","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_one","content":[{"type":"text","text":"tool result"},{"type":"file","file":{"filename":"report.docx","file_data":"data:application/vnd.openxmlformats-officedocument.wordprocessingml.document;base64,` + data + `"}}]}]}`
	prepared, documents, err := providerapi.PrepareKiroDocuments(contract.ProtocolOpenAIChat, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	converted, err := relaykitbridge.NewEngine().ConvertRequest(t.Context(), relaykitbridge.ConvertRequestInput{From: contract.ProtocolOpenAIChat, To: contract.ProtocolAnthropicMessages, ContentType: "application/json", Body: prepared, PublicModel: "m", UpstreamModel: "m"})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := documents.Restore(converted.Body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(string(restored)))
	if _, err := providerapi.KiroRequest(req, "", ""); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(req.Body)
	current := gjson.GetBytes(out, "conversationState.currentMessage.userInputMessage")
	if current.Get("documents.0.source.bytes").String() != data || current.Get("documents.0.format").String() != "docx" || current.Get("userInputMessageContext.toolResults.0.toolUseId").String() != "call_one" || !strings.Contains(current.Get("userInputMessageContext.toolResults.0.content.0.text").String(), "tool result") {
		t.Fatalf("tool document/result lost: %s", out)
	}
}
