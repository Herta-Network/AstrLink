package controlapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

func TestKiroAuthorizationOptionsAndManualCompletionThroughControlAPI(t *testing.T) {
	for _, method := range []string{"social", "iam_sso"} {
		t.Run(method, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/client/register" {
					io.WriteString(w, `{"clientId":"public-client","clientSecret":"private-registration","clientSecretExpiresAt":4102444800}`)
					return
				}
				if r.URL.Path == "/oauth/token" || r.URL.Path == "/token" {
					io.WriteString(w, `{"accessToken":"private-access","refreshToken":"private-refresh","profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/test","expiresIn":3600}`)
					return
				}
				t.Errorf("unexpected request %s", r.URL.Path)
				http.NotFound(w, r)
			}))
			defer upstream.Close()
			store, err := openControlTestStore(t, context.Background(), t.TempDir()+"/kiro-auth.sqlite")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			credentials := accountauth.NewMemoryCredentialStore()
			manager, err := subscription.NewManager(subscription.StorageAccountStore{Store: store}, credentials, accountauth.OAuthConfig{}, accountauth.OAuthConfig{Provider: contract.SubscriptionProviderKiro, Issuer: upstream.URL, APIBaseURL: upstream.URL, KiroPortalURL: upstream.URL + "/signin", KiroSocialBaseURL: upstream.URL, NoLoopbackCallback: true, HTTPClient: upstream.Client()})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewWithDependencies(contract.DefaultVersionResponse("test", "abc1234"), Dependencies{ServiceStore: store, Subscriptions: manager, ControlToken: testControlToken})
			if err != nil {
				t.Fatal(err)
			}
			call := func(verb, path, body string, status int) []byte {
				t.Helper()
				r := httptest.NewRequest(verb, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+testControlToken)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != status {
					t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
				}
				for _, secret := range []string{"private-access", "private-refresh", "private-registration"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Fatal("secret leaked to control response")
					}
				}
				return w.Body.Bytes()
			}
			var service contract.Service
			_ = json.Unmarshal(call("POST", ServicesPath, `{"name":"Kiro","kind":"kiro_subscription"}`, 201), &service)
			path := ServicesPath + "/" + string(service.ID) + "/authorization"
			call("POST", path, `{"flow":"browser","kiro":{"method":"iam_sso","region":"us-east-1.evil","start_url":"https://company.awsapps.com/start"}}`, 422)
			call("POST", path, `{"flow":"browser","kiro":{"method":"iam_sso","start_url":"http://company.awsapps.com/start"}}`, 422)
			call("POST", path, `{"flow":"browser","kiro":{"method":"social","token":"must-not-be-accepted"}}`, 422)
			options := contract.KiroAuthorization{Method: method}
			if method == "iam_sso" {
				options.Region = "eu-west-1"
				options.StartURL = "https://company.awsapps.com/start"
			}
			input, _ := json.Marshal(map[string]any{"flow": "browser", "kiro": options})
			var session contract.AuthorizationSession
			_ = json.Unmarshal(call("POST", path, string(input), 202), &session)
			if session.Flow != contract.AuthorizationFlowCode {
				t.Fatalf("remote browser session flow=%s", session.Flow)
			}
			auth, _ := url.Parse(session.AuthorizationURL)
			callback := auth.Query().Get("redirect_uri")
			if method == "social" {
				callback += "/oauth/callback"
			}
			callback += "?code=callback-secret&state=" + auth.Query().Get("state")
			complete, _ := json.Marshal(map[string]string{"session_id": string(session.ID), "code": callback})
			_ = json.Unmarshal(call("PUT", path, string(complete), 200), &session)
			if session.Status != contract.AuthorizationSessionStatusCompleted {
				t.Fatal("authorization did not complete")
			}
			tokens, err := credentials.Get(context.Background(), service.ID)
			if err != nil || tokens.KiroAuthMethod != method {
				t.Fatalf("credential method=%s err=%v", tokens.KiroAuthMethod, err)
			}
			call("PUT", path, string(complete), 409)
			call("POST", ServicesPath+"/"+string(service.ID)+"/logout", "", 200)
			if _, err := credentials.Get(context.Background(), service.ID); err == nil {
				t.Fatal("logout retained Kiro credentials")
			}
		})
	}
}
