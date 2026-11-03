package accountauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

type copilotGitHub struct {
	polls       atomic.Int32
	entitlement atomic.Int32 // HTTP status of /copilot_internal/user
	token       atomic.Value // the poll's terminal error, or "" for a token
}

func newCopilotGitHub(t *testing.T) (*copilotGitHub, *httptest.Server) {
	t.Helper()
	github := &copilotGitHub{}
	github.entitlement.Store(http.StatusOK)
	github.token.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("User-Agent") != "opencode/"+accountauth.DefaultCopilotClientVersion {
			t.Errorf("%s User-Agent = %q", request.URL.Path, request.Header.Get("User-Agent"))
		}
		for name := range request.Header {
			if strings.Contains(strings.ToLower(name+request.Header.Get(name)), "astrlink") {
				t.Errorf("%s carries gateway branding: %s", request.URL.Path, name)
			}
		}
		var body map[string]string
		if request.Method == http.MethodPost {
			if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
				t.Errorf("%s is not a JSON exchange: %v", request.URL.Path, request.Header)
			}
			raw, _ := io.ReadAll(request.Body)
			_ = json.Unmarshal(raw, &body)
			if body["client_id"] != accountauth.DefaultCopilotClientID {
				t.Errorf("%s client_id = %q", request.URL.Path, body["client_id"])
			}
		}
		switch request.URL.Path {
		case "/login/device/code":
			if body["scope"] != "read:user" {
				t.Errorf("scope = %q", body["scope"])
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"device_code": "device-code-secret", "user_code": "ABCD-1234",
				"verification_uri": "https://github.com/login/device", "expires_in": 899, "interval": 0,
			})
		case "/login/oauth/access_token":
			if body["device_code"] != "device-code-secret" || body["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("token body = %v", body)
			}
			// GitHub answers every poll with 200, the outcome in the body.
			switch github.polls.Add(1) {
			case 1:
				io.WriteString(writer, `{"error":"authorization_pending"}`)
			case 2:
				io.WriteString(writer, `{"error":"slow_down","interval":10}`)
			default:
				if failure := github.token.Load().(string); failure != "" {
					io.WriteString(writer, `{"error":"`+failure+`","error_description":"device-code-secret"}`)
					return
				}
				io.WriteString(writer, `{"access_token":"gho_copilot_secret","token_type":"bearer","scope":"read:user"}`)
			}
		case "/user":
			if request.Header.Get("Authorization") != "token gho_copilot_secret" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(writer, `{"login":"octocat","id":583231,"email":"octocat@example.com"}`)
		case "/copilot_internal/user":
			if request.Header.Get("Authorization") != "token gho_copilot_secret" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			if status := int(github.entitlement.Load()); status != http.StatusOK {
				writer.WriteHeader(status)
				return
			}
			io.WriteString(writer, `{"copilot_plan":"individual_pro","quota_reset_date":"2026-11-01"}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return github, server
}

func copilotTestConfig(server *httptest.Server) accountauth.OAuthConfig {
	return accountauth.OAuthConfig{
		Provider:              contract.SubscriptionProviderGitHubCopilot,
		Issuer:                server.URL,
		UserInfoURL:           server.URL + "/user",
		UsageURL:              server.URL + "/copilot_internal/user",
		HTTPClient:            server.Client(),
		DevicePollMinInterval: 5 * time.Millisecond,
		DevicePollMaxInterval: 20 * time.Millisecond,
	}
}

func TestCopilotDeviceCodeLoginStoresTheGitHubTokenAsOpenCode(t *testing.T) {
	github, server := newCopilotGitHub(t)
	store := accountauth.NewMemoryCredentialStore()
	config := copilotTestConfig(server)
	manager := accountauth.NewSessionManager(config, store, func(ctx context.Context, session contract.AuthorizationSession, tokens accountauth.AccountTokens) error {
		return store.Put(ctx, session.ServiceID, tokens)
	})
	for _, flow := range []contract.AuthorizationFlow{contract.AuthorizationFlowBrowser, contract.AuthorizationFlowCode} {
		if _, err := manager.Begin(context.Background(), "service_copilot", flow, nil); err == nil {
			t.Fatalf("%s flow accepted for Copilot", flow)
		}
	}
	session, err := manager.Begin(context.Background(), "service_copilot", contract.AuthorizationFlowDeviceCode, nil)
	if err != nil {
		t.Fatalf("Begin() = %v", err)
	}
	if session.Provider != contract.SubscriptionProviderGitHubCopilot || session.DeviceCode == nil ||
		session.DeviceCode.UserCode != "ABCD-1234" || session.DeviceCode.VerificationURL != "https://github.com/login/device" {
		t.Fatalf("device session = %#v", session)
	}
	if err := session.Validate(); err != nil {
		t.Fatalf("session invalid: %v", err)
	}
	waitForSessionStatus(t, manager, "service_copilot", contract.AuthorizationSessionStatusCompleted)
	if github.polls.Load() != 3 {
		t.Fatalf("polls = %d", github.polls.Load())
	}
	tokens, err := store.Get(context.Background(), "service_copilot")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "gho_copilot_secret" || tokens.RefreshToken != "gho_copilot_secret" ||
		tokens.AccountID != "583231" || tokens.PlanType != "individual_pro" ||
		tokens.ExpiresAt.Before(time.Now().AddDate(1, 0, 0)) {
		t.Fatalf("stored tokens = %#v", tokens)
	}

	client := accountauth.NewTokenClient(config)
	checked, err := client.Refresh(context.Background(), tokens.RefreshToken)
	if err != nil || checked.AccessToken != "gho_copilot_secret" || checked.RefreshToken != "gho_copilot_secret" {
		t.Fatalf("Refresh() = %#v, %v", checked, err)
	}
	if _, err := client.Refresh(context.Background(), "gho_revoked"); !errors.Is(err, accountauth.ErrInvalidGrant) {
		t.Fatalf("Refresh(revoked) = %v, want ErrInvalidGrant", err)
	}
}

func TestCopilotLoginFailsWithoutAPlanOrApproval(t *testing.T) {
	for _, tt := range []struct {
		name, tokenError, code string
		entitlement            int
	}{
		{name: "no plan", entitlement: http.StatusNotFound, code: accountauth.ErrCodeCopilotNotEntitled},
		{name: "denied", tokenError: "access_denied", entitlement: http.StatusOK, code: accountauth.ErrCodeDeviceCodePoll},
	} {
		t.Run(tt.name, func(t *testing.T) {
			github, server := newCopilotGitHub(t)
			github.entitlement.Store(int32(tt.entitlement))
			github.token.Store(tt.tokenError)
			store := accountauth.NewMemoryCredentialStore()
			manager := accountauth.NewSessionManager(copilotTestConfig(server), store, func(ctx context.Context, session contract.AuthorizationSession, tokens accountauth.AccountTokens) error {
				return store.Put(ctx, session.ServiceID, tokens)
			})
			if _, err := manager.Begin(context.Background(), "service_copilot", contract.AuthorizationFlowDeviceCode, nil); err != nil {
				t.Fatalf("Begin() = %v", err)
			}
			waitForSessionStatus(t, manager, "service_copilot", contract.AuthorizationSessionStatusFailed)
			failed, _ := manager.Get("service_copilot")
			if failed.Error == nil || failed.Error.Code != tt.code || strings.Contains(failed.Error.Message, "secret") {
				t.Fatalf("failed session = %#v", failed.Error)
			}
			if _, err := store.Get(context.Background(), "service_copilot"); !errors.Is(err, accountauth.ErrCredentialNotFound) {
				t.Fatalf("a failed login stored tokens: %v", err)
			}
		})
	}
}

func TestApplyCopilotAPIHeadersUsesOpenCodeIdentity(t *testing.T) {
	headers := make(http.Header)
	accountauth.ApplyCopilotAPIHeaders(headers, accountauth.AccountTokens{AccessToken: "gho_tok"})
	if headers.Get("Authorization") != "Bearer gho_tok" ||
		headers.Get("User-Agent") != "opencode/"+accountauth.DefaultCopilotClientVersion ||
		headers.Get("X-Github-Api-Version") != "2026-06-01" || headers.Get("Openai-Intent") != "conversation-edits" {
		t.Fatalf("headers = %v", headers)
	}
	normalized := accountauth.OAuthConfig{Provider: contract.SubscriptionProviderGitHubCopilot}.Normalize()
	if normalized.ClientID != accountauth.DefaultCopilotClientID || normalized.Issuer != "https://github.com" ||
		normalized.TokenURL != "https://github.com/login/oauth/access_token" ||
		normalized.APIBaseURL != "https://api.githubcopilot.com" ||
		normalized.UserInfoURL != "https://api.github.com/user" ||
		normalized.UsageURL != "https://api.github.com/copilot_internal/user" ||
		strings.Join(normalized.Scopes, " ") != "read:user" {
		t.Fatalf("normalized = %#v", normalized)
	}
}
