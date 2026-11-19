package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

type redirectServices []contract.Service

func (services redirectServices) ListServices(context.Context, storage.ServiceListOptions) (storage.ServicePage, error) {
	page := storage.ServicePage{}
	for _, service := range services {
		page.Items = append(page.Items, storage.ServiceRecord{Service: service})
	}
	return page, nil
}

// A Claude Code request keeps its model while routing, so it can fail over
// from a Claude subscription to Copilot, whose built-in rule sends its dotted
// name.
func TestClaudeCodeModelFailsOverToCopilotUnderItsDottedName(t *testing.T) {
	subscription := func(id contract.ServiceID, kind contract.ServiceKind, models ...string) contract.Service {
		return contract.Service{
			ID: id, Name: string(id), Kind: kind, Enabled: true, Models: models,
			Capabilities: kind.SubscriptionProvider().Capabilities(),
			Subscription: &contract.SubscriptionConnection{
				Provider: kind.SubscriptionProvider(), Status: contract.SubscriptionStatusConnected,
				CredentialRef: "local://subscription/" + string(id),
			},
		}
	}
	copilot := subscription("service_b_copilot", contract.ServiceKindCopilotSubscription, "claude-sonnet-4.6")
	resolver, err := endpoint.NewStoreResolver(redirectServices{
		subscription("service_a_claude", contract.ServiceKindClaudeSubscription, "claude-sonnet-4-6"),
		copilot,
	})
	if err != nil {
		t.Fatal(err)
	}
	var hosts, models []string
	handler := NewWithDependencies(Dependencies{
		Resolver:   resolver,
		Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}),
		Forwarder: transport.New(roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			hosts = append(hosts, request.URL.Host)
			models = append(models, ojson.GetBytes(body, "model").String())
			if request.URL.Host == "api.anthropic.com" {
				return jsonResponse(http.StatusServiceUnavailable, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), nil
			}
			return jsonResponse(http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
		})),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	if strings.Join(hosts, ",") != "api.anthropic.com,api.githubcopilot.com" ||
		strings.Join(models, ",") != "claude-sonnet-4-6,claude-sonnet-4.6" {
		t.Fatalf("attempts = %v, models = %v", hosts, models)
	}
}

func TestKiroClaude55AliasIsDiscoverableAndReachesBothRuntimeAPIs(t *testing.T) {
	kiro := kiroSettingsCandidate("service_kiro", "").Service
	kiro.Models = []string{"claude-sonnet-5.5", "claude-haiku-5.5"}
	kiro.ModelRedirects = []contract.ModelRedirect{{From: "claude-haiku-5-5", To: "claude-haiku-5.5", Enabled: false}}
	// A second provider listing Opus must not activate Kiro's Opus alias.
	droid := contract.Service{ID: "service_droid", Name: "Droid", Enabled: true, Kind: contract.ServiceKindDroidSubscription,
		Models: []string{"claude-opus-5.5"}, Capabilities: contract.SubscriptionProviderFactoryDroid.Capabilities(),
		Subscription: &contract.SubscriptionConnection{Provider: contract.SubscriptionProviderFactoryDroid, Status: contract.SubscriptionStatusConnected, CredentialRef: "local://subscription/service_droid"}}
	resolver, err := endpoint.NewStoreResolver(redirectServices{kiro, droid})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	handler := NewWithDependencies(Dependencies{ResolveKiro: kiroTestTokenLimit,
		Resolver: resolver, Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}),
		Forwarder: transport.New(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			var payload, media, model string
			switch req.URL.Path {
			case "/":
				if req.Header.Get("X-Amz-Target") != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
					t.Error("expected GAR")
				}
				model = ojson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String()
				media = "application/vnd.amazon.eventstream"
				payload = string(kiroOKFrames())
			case "/v1/responses":
				model = ojson.GetBytes(body, "model").String()
				media = "text/event-stream"
				payload = "data: " + `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[]}}` + "\n\n"
			default:
				t.Fatalf("unexpected upstream request %s", req.URL.Path)
			}
			if model != "claude-sonnet-5.5" {
				t.Errorf("upstream model = %q", model)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
		})),
	})
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest("GET", "/v1/models", nil))
	entries := ojson.GetBytes(list.Body.Bytes(), "data")
	if list.Code != 200 || calls != 0 || len(entries.Array()) != 4 {
		t.Fatalf("catalog: %d %s", list.Code, list.Body.String())
	}
	for _, id := range []string{"claude-sonnet-5-5", "claude-sonnet-5.5", "claude-haiku-5.5", "claude-opus-5.5"} {
		if !entries.Get(`#(id=="` + id + `")`).Exists() {
			t.Errorf("missing catalog entry %s", id)
		}
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"test"}],"max_tokens":128}`},
		{"/v1/responses", `{"model":"claude-sonnet-5-5","input":"test"}`},
	} {
		req := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d", calls)
	}
}
