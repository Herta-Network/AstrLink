package subscription

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

func TestKiroModelsPaginationAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "KiroIDE-0.13.0") || !strings.Contains(r.Header.Get("X-Amz-User-Agent"), "KiroIDE-0.13.0") {
			t.Errorf("catalog/usage identity not resolved: %v", r.Header)
		}
		var payload map[string]any
		if r.Method != http.MethodPost || r.URL.Path != "/" || r.Header.Get("Content-Type") != "application/x-amz-json-1.0" || json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("expected JSON RPC")
		}
		if r.Header.Get("Authorization") != "Bearer account-token" || payload["origin"] != "AI_EDITOR" || payload["profileArn"] != "test-profile" {
			t.Errorf("missing identity/query: %v %s", r.Header, r.URL)
		}
		for key, values := range r.Header {
			if strings.Contains(strings.ToLower(key+strings.Join(values, "")), "astrlink") {
				t.Errorf("gateway header escaped: %s", key)
			}
		}
		if r.Header.Get("X-Amz-Target") == "KiroControlPlaneBearerService.ListAvailableModels" {
			if r.URL.RawQuery != "" {
				t.Errorf("model catalog query: %s", r.URL.RawQuery)
			}
			if payload["nextToken"] == nil {
				if len(payload) != 2 {
					t.Errorf("unexpected first-page fields: %v", payload)
				}
				io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.6","promptCaching":{"supportsPromptCaching":true,"maximumCacheCheckpointsPerRequest":4,"minimumTokensPerCacheCheckpoint":1024},"additionalModelRequestFieldsSchema":{"properties":{"max_tokens":{"type":"integer"}}}}],"nextToken":"next+page"}`)
			} else {
				if len(payload) != 3 || payload["nextToken"] != "next+page" {
					t.Error("cursor not preserved")
				}
				io.WriteString(w, `{"models":[{"modelId":"claude-sonnet-4.6"},{"modelId":"claude-haiku-4.5"}]}`)
			}
		} else if r.Header.Get("X-Amz-Target") == "AmazonCodeWhispererService.GetUsageLimits" {
			if len(payload) != 3 || payload["isEmailRequired"] != false || len(r.URL.Query()) != 2 || r.URL.Query().Get("origin") != payload["origin"] || r.URL.Query().Get("isEmailRequired") != "false" {
				t.Errorf("unexpected usage RPC parameters: %v %s", payload, r.URL.RawQuery)
			}
			io.WriteString(w, `{"subscriptionInfo":{"subscriptionType":"PRO"},"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":20,"usageLimit":100,"freeTrialInfo":{"freeTrialStatus":"ACTIVE","currentUsage":5,"usageLimit":50},"bonuses":[{"status":"EXPIRED","currentUsage":9,"usageLimit":999}]}]}`)
		} else {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager, err := NewManager(NewMemoryAccountStore(), accountauth.NewMemoryCredentialStore(), accountauth.OAuthConfig{}, accountauth.OAuthConfig{Identities: accountauth.NewIdentityRegistry(kiroIdentitySettings{}, nil), Provider: contract.SubscriptionProviderKiro, APIBaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tokens := accountauth.AccountTokens{AccessToken: "account-token", ProjectID: "test-profile"}
	models, err := manager.KiroModels(context.Background(), tokens)
	if err != nil || strings.Join(models, ",") != "claude-haiku-4.5,claude-sonnet-4.6" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	catalog, err := manager.kiroModelCatalog(t.Context(), tokens)
	if err != nil {
		t.Fatal(err)
	}
	cached := catalog["claude-sonnet-4.6"]
	if !cached.PromptCaching.SupportsPromptCaching || !hasCacheCount(cached.PromptCaching.MaximumCacheCheckpointsPerRequest, 4) || !hasCacheCount(cached.PromptCaching.MinimumTokensPerCacheCheckpoint, 1024) || len(cached.ModelSchema) == 0 {
		t.Fatalf("capabilities lost across pagination: %+v", cached)
	}
	if catalog["claude-haiku-4.5"].PromptCaching.SupportsPromptCaching {
		t.Fatal("model without caching metadata acquired another model's capabilities")
	}
	usage, err := manager.kiroUsage(context.Background(), tokens)
	if err != nil || usage.PlanType != "PRO" || usage.Primary == nil || usage.Primary.UsedPercent != 100.0*25/150 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}

func TestKiroUsageRejectsUnknownOrNegativeAllowance(t *testing.T) {
	for _, body := range []string{`{}`, `{"usageBreakdownList":[]}`, `{"usageBreakdownList":[{"resourceType":"CREDIT","currentUsage":-1,"usageLimit":20}]}`} {
		if _, err := DecodeKiroUsage([]byte(body), time.Now()); err == nil {
			t.Fatalf("accepted invalid usage: %s", body)
		}
	}
}

type kiroIdentitySettings struct{}

func (kiroIdentitySettings) GetRoutingSettings(context.Context) (contract.RoutingSettings, error) {
	s := contract.DefaultRoutingSettings()
	s.KiroIdentityVersion = "0.13.0"
	return s, nil
}

func hasCacheCount(got *int, want int) bool { return got != nil && *got == want }

func TestKiroCatalogPreservesOptionalLimitsAndRejectsInvalidLimits(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		valid        bool
	}{
		{"unspecified", "", true},
		{"explicit zero", `,"maximumCacheCheckpointsPerRequest":0,"minimumTokensPerCacheCheckpoint":0`, true},
		{"negative maximum", `,"maximumCacheCheckpointsPerRequest":-1`, false},
		{"negative minimum", `,"minimumTokensPerCacheCheckpoint":-1`, false},
		{"out of range", `,"maximumCacheCheckpointsPerRequest":2147483648`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"models":[{"modelId":"m","promptCaching":{"supportsPromptCaching":true`+test.fields+`}}]}`)
			}))
			defer server.Close()
			m, err := NewManager(NewMemoryAccountStore(), accountauth.NewMemoryCredentialStore(), accountauth.OAuthConfig{}, accountauth.OAuthConfig{Provider: contract.SubscriptionProviderKiro, APIBaseURL: server.URL, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := m.kiroModelCatalog(t.Context(), accountauth.AccountTokens{AccessToken: "test-grant", ProjectID: "profile"})
			if !test.valid {
				if err == nil {
					t.Fatal("invalid catalog accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c := catalog["m"].PromptCaching
			if test.fields == "" {
				if c.MaximumCacheCheckpointsPerRequest != nil || c.MinimumTokensPerCacheCheckpoint != nil {
					t.Fatal("absent limit acquired a value")
				}
			} else if !hasCacheCount(c.MaximumCacheCheckpointsPerRequest, 0) || !hasCacheCount(c.MinimumTokensPerCacheCheckpoint, 0) {
				t.Fatal("explicit zero disappeared")
			}
		})
	}
}
