package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func TestKiroEntrySelectsWireIndependentlyOfAccountFlags(t *testing.T) {
	const profile = "arn:aws:codewhisperer:us-east-1:123456789012:profile/unit"
	var features, catalogs, responses, gar atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grant := r.Header.Get("Authorization")
		if grant != "Bearer a" && grant != "Bearer b" && grant != "Bearer denied" {
			t.Errorf("credential not bound to selected account")
		}
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-astrlink-") {
				t.Errorf("local header sent: %s", key)
			}
		}
		switch {
		case r.Header.Get("X-Amz-Target") == "AmazonCodeWhispererStreamingService.GetFeatureConfiguration":
			features.Add(1)
			var input map[string]any
			if r.Method != http.MethodPost || r.URL.Path != "/" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/x-amz-json-1.0" || r.Header.Get("X-Amzn-Kiro-Profile-Arn") != "" || json.NewDecoder(r.Body).Decode(&input) != nil || len(input) != 3 || input["profileArn"] != profile || input["origin"] != "AI_EDITOR" || input["version"] != "0.13.0" {
				t.Errorf("wrong feature RPC: %s %s %v", r.Method, r.URL, input)
			}
			if grant == "Bearer denied" {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			main, aux, field := "disabled", "disabled", true
			if grant == "Bearer a" {
				main = "create_response"
				field = false
			}
			json.NewEncoder(w).Encode(map[string]any{"configuration": map[string]any{"8242f5d7e306898aef7ec21ccbe97fd8e9150efbe8f093b12c8dc8b7d46fb604": main, "d569f7177d4ed245e41336ec6eb9bdd58d6fb56e4c4701f71ea809c9b8ffaa8e": aux, "2baac88264069c460f17175771938783595b0a429862b257a97735a0c6e7c03a": field}})
		case r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels":
			catalogs.Add(1)
			io.WriteString(w, `{"models":[{"modelId":"m","tokenLimits":{"maxOutputTokens":64000},"promptCaching":{"supportsPromptCaching":true,"maximumCacheCheckpointsPerRequest":4,"minimumTokensPerCacheCheckpoint":1024},"additionalModelRequestFieldsSchema":{"properties":{"max_tokens":{"type":"integer","minimum":1024,"maximum":64000},"reasoning":{"properties":{"effort":{"type":"string","enum":["high"]}}}}}}]}`)
		case r.URL.Path == "/v1/responses":
			responses.Add(1)
			raw, _ := io.ReadAll(r.Body)
			root := ojson.ParseBytes(raw)
			if r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("X-Amzn-Kiro-Profile") != profile || r.Header.Get("X-Amz-Target") != "" || root.Get("instructions").Exists() != (grant == "Bearer b") || (grant == "Bearer a" && root.Get("input.0.content").String() != "my-system") || root.Get("reasoning.effort").String() != "high" {
				t.Errorf("Responses request differs: %s", raw)
			}
			if root.Get("input.0.content").String() == "refuse this request" {
				http.Error(w, "CreateResponse refused", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"type":"response.output_text.delta","output_index":0,"delta":"answer"}

data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":10,"output_tokens":1}}}

`)
		case r.URL.Path == "/" && r.Header.Get("X-Amz-Target") == "AmazonCodeWhispererStreamingService.GenerateAssistantResponse":
			gar.Add(1)
			raw, _ := io.ReadAll(r.Body)
			root := ojson.ParseBytes(raw)
			if r.Header.Get("Content-Type") != "application/x-amz-json-1.0" || root.Get("additionalModelRequestFields.reasoning.effort").String() != "high" || root.Get("systemPrompt").Exists() != (grant == "Bearer b") || strings.Count(string(raw), "my-system") != 1 {
				t.Errorf("GAR request differs: %s", raw)
			}
			// Return a provider refusal; forwarding it once is also how a disabled
			// system field is handled if the account assignment changed mid-request.
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"request refused"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m, err := NewManager(NewMemoryAccountStore(), accountauth.NewMemoryCredentialStore(), accountauth.OAuthConfig{}, accountauth.OAuthConfig{Provider: contract.SubscriptionProviderKiro, APIBaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(server.URL)
	f := transport.New(server.Client().Transport)
	resolve := func(ctx context.Context, header http.Header, model string, protocol contract.ProtocolID) (providerapi.KiroRuntime, error) {
		runtime, err := m.KiroRuntime(ctx, header, model, protocol)
		if err == nil && protocol == contract.ProtocolAnthropicMessages && (runtime.MaxOutputTokens != 64000 || !runtime.PromptCaching.SupportsPromptCaching || !hasCacheCount(runtime.PromptCaching.MaximumCacheCheckpointsPerRequest, 4) || !hasCacheCount(runtime.PromptCaching.MinimumTokensPerCacheCheckpoint, 1024)) {
			t.Errorf("Kiro inference did not receive the model's caching contract: %+v", runtime.PromptCaching)
		}
		return runtime, err
	}
	for _, tt := range []struct {
		token     string
		responses bool
		want      int
	}{{"a", true, 200}, {"a", false, 400}, {"b", true, 200}, {"b", true, 403}, {"b", false, 400}, {"denied", true, 0}} {
		header := make(http.Header)
		providerapi.ApplyKiroHeaders(header, tt.token, providerapi.KiroIdentityAt("0.13.0", true))
		header.Set(providerapi.KiroProfileHeader, profile)
		req := httptest.NewRequest("POST", "http://localhost/v1/messages", strings.NewReader(`{"model":"m","system":"my-system","messages":[{"role":"user","content":"hello"}],"max_tokens":1024,"output_config":{"effort":"high"}}`))
		if tt.responses {
			req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"m","input":"hello","instructions":"my-system","reasoning":{"effort":"high"}}`))
		}
		if tt.want == 403 {
			req = httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{"model":"m","input":"refuse this request","instructions":"my-system","reasoning":{"effort":"high"}}`))
		}
		req.Header.Set("Authorization", "Bearer caller")
		req.Header.Set("X-AstrLink-Kiro-Profile", "caller-supplied")
		res, err := f.RoundTrip(req, transport.Target{Service: contract.Service{ID: "service_a", Kind: contract.ServiceKindKiroSubscription}, BaseURL: base, RequestHeaders: header, Kiro: &transport.KiroAttempt{Resolve: resolve, Model: "m"}})
		if tt.want == 0 {
			if err == nil {
				res.Body.Close()
				t.Fatal("sent inference after configuration refusal")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != tt.want {
			t.Fatalf("status=%d body=%s err=%v", res.StatusCode, raw, err)
		}
		if tt.want == 200 && ojson.GetBytes(raw, "output.0.content.0.text").String() != "answer" {
			t.Fatalf("bad response: %s", raw)
		}
	}
	if features.Load() != 3 || catalogs.Load() != 2 || responses.Load() != 3 || gar.Load() != 2 {
		t.Fatalf("features=%d catalogs=%d responses=%d gar=%d", features.Load(), catalogs.Load(), responses.Load(), gar.Load())
	}
}

func TestKiroCacheCancellationDoesNotCrossRequests(t *testing.T) {
	var c kiroCache[int]
	key := [32]byte{1}
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := c.get(t.Context(), key, func() (int, error) { close(started); <-release; return 42, nil })
		result <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	unexpected := func() (int, error) { return 0, fmt.Errorf("duplicate metadata fetch") }
	_, err := c.get(ctx, key, unexpected)
	close(release)
	if <-result != nil || err != context.Canceled {
		t.Fatalf("waiter error: %v", err)
	}
	value, err := c.get(t.Context(), key, unexpected)
	if err != nil || value != 42 {
		t.Fatalf("cached result %d %v", value, err)
	}
}
