package ingress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/privacy"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/tidwall/gjson"
)

func kiroSettingsCandidate(id contract.ServiceID, base string) endpoint.Resolved {
	c := protectionClaudeCandidate(id, base)
	c.Mode = contract.CapabilityModeAdapter
	c.PlanType = contract.PlanTypeAdapter
	c.Service.Kind = contract.ServiceKindKiroSubscription
	c.Service.Subscription.Provider = contract.SubscriptionProviderKiro
	c.Service.Capabilities = contract.SubscriptionProviderKiro.Capabilities()
	c.UpstreamModel = "claude-sonnet-4-5"
	return c
}

func kiroOKFrames() []byte {
	return append(kiroWireFrame("assistantResponseEvent", []byte(`{"content":"ok"}`)), kiroWireFrame("meteringEvent", []byte(`{"inputTokens":3,"outputTokens":1}`))...)
}

func TestKiroIdentitySettingsReachTheFinalUpstreamRequest(t *testing.T) {
	for _, converted := range []bool{false, true} {
		for _, learn := range []bool{false, true} {
			for _, enforce := range []bool{false, true} {
				t.Run(fmt.Sprintf("converted=%t/learn=%t/enforce=%t", converted, learn, enforce), func(t *testing.T) {
					upstream := newCapturedUpstream(t, 200, "application/vnd.amazon.eventstream", string(kiroOKFrames()))
					store := newRedirectSettingsStore()
					store.settings.KiroIdentityAutoLearn = learn
					store.settings.KiroIdentityEnforcement = enforce
					registry := accountauth.NewIdentityRegistry(store, nil)
					candidate := kiroSettingsCandidate("service_kiro", upstream.url)
					path := "/v1/messages"
					if converted {
						path = "/v1/chat/completions"
						candidate.Service.Capabilities = append(candidate.Service.Capabilities, contract.Capability{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeAdapter, Streaming: true, ConvertTo: contract.ProtocolAnthropicMessages})
					}
					handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{candidate}}, RequestRecords: store, Identities: registry, ConversionEngine: relaykitbridge.NewEngine(), Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{}).WithRoutingSettings(store).WithIdentities(registry)})
					req := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
					req.Header.Set("Content-Type", "application/json")
					providerapi.ApplyKiroIdentity(req.Header, providerapi.KiroIdentityAt("0.14.0", true))
					req.Header.Set("X-AstrLink-Injected", "private")
					req.Header.Set("Originator", "AstrLink")
					res := httptest.NewRecorder()
					handler.ServeHTTP(res, req)
					if res.Code != 200 {
						t.Fatalf("status=%d %s", res.Code, res.Body.String())
					}
					h, _ := upstream.request(t)
					want := accountauth.DefaultKiroClientVersion
					if !converted && (learn || !enforce) {
						want = "0.14.0"
					}
					identity, ok := providerapi.RecognizeKiroIdentity(h)
					if !ok || identity.Version != want {
						t.Fatalf("final headers=%v want=%s", h, want)
					}
					learned := registry.ClientIdentities().Kiro.LearnedVersion
					if (learned != "") != (learn && !converted) {
						t.Fatalf("unexpected learned version: %s", learned)
					}
				})
			}
		}
	}
}

func TestKiroSessionIsolationIsStableAndCanBeDisabled(t *testing.T) {
	const sessionID = "12345678-1234-4234-8234-123456789abc"
	upstream := newCapturedUpstream(t, 200, "application/vnd.amazon.eventstream", string(kiroOKFrames()))
	send := func(id contract.ServiceID, isolate bool) string {
		settings := contract.DefaultRoutingSettings()
		settings.SubscriptionSessionIsolation = isolate
		body := `{"model":"claude-sonnet-4-5","metadata":{"session_id":"` + sessionID + `"},"messages":[{"role":"user","content":"hello"}]}`
		res := serveProtectedRequest(t, settings, nil, kiroSettingsCandidate(id, upstream.url), "/v1/messages", body, nil)
		if res.Code != 200 {
			t.Fatalf("%d %s", res.Code, res.Body.String())
		}
		_, raw := upstream.request(t)
		return gjson.GetBytes(raw, "conversationState.conversationId").String()
	}
	a := send("service_a", true)
	if a == sessionID || a != send("service_a", true) || a == send("service_b", true) {
		t.Fatal("session mapping is not stable and account scoped")
	}
	if send("service_a", false) != sessionID || send("service_b", false) != sessionID {
		t.Fatal("disabled isolation changed caller session")
	}
}

func TestKiroThinkingRepairRespectsSwitchAndAttemptBudget(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("opaque=%t/enabled=%t", opaque, enabled), func(t *testing.T) {
				var sent []string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					sent = append(sent, string(raw))
					if !opaque && len(sent) == 1 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(400)
						io.WriteString(w, `{"message":"Invalid signature in thinking block"}`)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
					_, _ = w.Write(kiroOKFrames())
				}))
				defer upstream.Close()
				candidate := kiroSettingsCandidate("service_kiro", upstream.URL)
				policy := contract.DefaultFailurePolicy()
				policy.ThinkingSignatureRecovery = &enabled
				policy.MaxRetries = 0
				policy.HTTPStatus["400"] = contract.FailureStop
				policy.InitialDelayMS = 60000
				policy.MaxDelayMS = 60000
				candidate.FailurePolicy = &policy
				candidate.Failover = &contract.FailoverPolicy{Enabled: false, Strategy: contract.FailoverOnly, MaxAttempts: 2}
				thought := `{"type":"thinking","thinking":"keep this reasoning"}`
				if opaque {
					thought = `{"type":"redacted_thinking","data":"foreign-ciphertext"}`
				}
				body := `{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":1024},"max_tokens":2048,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[` + thought + `,{"type":"text","text":"keep this answer"}]},{"role":"user","content":"continue"}]}`
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{candidate}}, Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{})})
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)).WithContext(ctx)
				req.Header.Set("Content-Type", "application/json")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				want := 1
				if opaque {
					want = 0
				}
				if enabled {
					want++
				}
				if len(sent) != want || ctx.Err() != nil || (res.Code == 200) != enabled {
					t.Fatalf("sent=%d want=%d status=%d err=%v body=%s", len(sent), want, res.Code, ctx.Err(), res.Body.String())
				}
				if enabled {
					last := sent[len(sent)-1]
					if strings.Contains(last, "foreign-ciphertext") || strings.Contains(last, "<thinking>") || !strings.Contains(last, "keep this answer") || !strings.Contains(last, "continue") {
						t.Fatalf("repair lost content or retained incompatible thinking: %s", last)
					}
					if !opaque && !strings.Contains(last, "keep this reasoning") {
						t.Fatal("readable reasoning was lost")
					}
				}
			})
		}
	}
}

func TestKiroRiskClassificationDoesNotConfuseThrottlingWithExhaustion(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		status int
		body   string
		scope  subscriptionRiskScope
		code   string
	}{
		{403, `{"reason":"TEMPORARILY_SUSPENDED","message":"Account temporarily suspended"}`, subscriptionRiskAccount, contract.RiskCodeAccountDeactivated},
		{429, `{"reason":"MONTHLY_REQUEST_COUNT","message":"usage limit exceeded"}`, subscriptionRiskAccount, contract.RiskCodeUsageLimitReached},
		{429, `{"__type":"ThrottlingException","message":"try again"}`, subscriptionRiskModel, ""},
		{429, `{"reason":"INSUFFICIENT_MODEL_CAPACITY"}`, subscriptionRiskModel, ""},
		{400, `{"message":"invalid request containing monthly request text"}`, subscriptionRiskNone, ""},
		{403, `<html>challenge</html>`, subscriptionRiskNone, ""},
		{403, `{"__type":"AccessDeniedException","message":"not authorized for this model"}`, subscriptionRiskNone, ""},
	} {
		signal := classifySubscriptionRisk(contract.ServiceKindKiroSubscription, test.status, http.Header{}, []byte(test.body), now)
		if signal.scope != test.scope || signal.observation.Code != test.code {
			t.Fatalf("%d %s: %+v", test.status, test.body, signal)
		}
	}
}

func TestKiroRiskSwitchCoversHTTPAndBinaryStreamExceptions(t *testing.T) {
	for _, binaryStream := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			reporter := newRecordingSubscriptionRisk()
			body := `{"reason":"TEMPORARILY_SUSPENDED","message":"Account temporarily suspended"}`
			status, contentType := 403, "application/json"
			if binaryStream {
				status = 200
				contentType = "application/vnd.amazon.eventstream"
				body = string(kiroWireTypedFrame("exception", "AccessDeniedException", []byte(body)))
			}
			upstream := newCapturedUpstream(t, status, contentType, body)
			settings := contract.DefaultRoutingSettings()
			settings.SubscriptionRiskProtection = enabled
			res := serveProtectedRequest(t, settings, reporter, kiroSettingsCandidate("service_kiro", upstream.url), "/v1/messages", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`, nil)
			if res.Code == 200 {
				t.Fatal("provider failure reported success")
			}
			reports, _ := reporter.snapshot()
			if (len(reports) > 0) != enabled {
				t.Fatalf("binary=%t enabled=%t reports=%v", binaryStream, enabled, reports)
			}
		}
	}
}

func TestKiroSignedThinkingRepairRunsPrivacyAgain(t *testing.T) {
	const secret = "k8Jd2nQ7xR4mT9pL3vZ6"
	for _, enabled := range []bool{false, true} {
		calls := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "foreign-signature") {
				t.Error("signed history bypassed privacy after repair")
			}
			if !strings.Contains(string(raw), "api_key=") {
				t.Error("readable thinking text was discarded")
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroOKFrames())
		}))
		candidate := kiroSettingsCandidate("service_kiro", upstream.URL)
		policy := contract.DefaultFailurePolicy()
		policy.ThinkingSignatureRecovery = &enabled
		candidate.FailurePolicy = &policy
		privacyPolicy, err := privacy.FromContractPolicy(contract.DefaultPrivacyPolicy())
		if err != nil {
			t.Fatal(err)
		}
		handler := NewWithDependencies(Dependencies{Resolver: candidateResolver{candidates: []endpoint.Resolved{candidate}}, PrivacyFilter: testPrivacyEngine(t, privacyPolicy, nil), Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, codingPlanCredentials{})})
		body := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"api_key=` + secret + `","signature":"foreign-signature"},{"type":"text","text":"answer"}]},{"role":"user","content":"continue"}]}`
		req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		upstream.Close()
		if enabled && (res.Code != 200 || calls != 1) || !enabled && (res.Code == 200 || calls != 0) {
			t.Fatalf("enabled=%t status=%d calls=%d: %s", enabled, res.Code, calls, res.Body.String())
		}
	}
}
