package accountauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"

	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// Kiro's Builder ID device grant registers its own public OIDC client.
// Wire reference: tanu360/kiro-reverse-api, a705464b, auth/builderid.go and
// auth/oidc.go. Client secrets are issued at login, never bundled in the app.
const (
	DefaultKiroIssuer     = "https://oidc.us-east-1.amazonaws.com"
	DefaultKiroAPIBaseURL = "https://codewhisperer.us-east-1.amazonaws.com"
	kiroStartURL          = "https://view.awsapps.com/start"
)

func normalizeKiroConfig(config OAuthConfig) OAuthConfig {
	if config.Issuer == "" {
		config.Issuer = DefaultKiroIssuer
	}
	if config.TokenURL == "" {
		config.TokenURL = strings.TrimRight(config.Issuer, "/") + "/token"
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = DefaultKiroAPIBaseURL
	}
	if config.DeviceCodeTTL <= 0 {
		config.DeviceCodeTTL = 10 * time.Minute
	}
	return config
}

func ApplyKiroAPIHeaders(header http.Header, tokens AccountTokens, identity ClientIdentity) {
	providerapi.ApplyKiroHeaders(header, tokens.AccessToken, providerapi.KiroIdentityAt(identity.Version, false))
	header[http.CanonicalHeaderKey(providerapi.KiroProfileHeader)] = nil
	if tokens.ProjectID != "" {
		header.Set(providerapi.KiroProfileHeader, tokens.ProjectID)
	}
}

// Refuse redirects so an OIDC response cannot redirect a client secret to
// another host. Never include provider response bodies in diagnostic errors.
func kiroJSON(ctx context.Context, config OAuthConfig, endpoint string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	providerapi.ApplyKiroHeaders(req.Header, "", providerapi.KiroIdentityAt(config.Identities.KiroIdentityFor(ctx).Version, false))
	req.Header.Set("Content-Type", "application/json")
	client := *config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("Kiro authorization request failed")
	}
	defer res.Body.Close()
	body, err = transport.ReadResponseBody(res, 1<<20)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid Kiro authorization response")
	}
	return body, res.StatusCode, nil
}

func (manager *SessionManager) requestKiroDeviceAuthorization(ctx context.Context, options contract.KiroAuthorization) (polledDeviceAuthorization, error) {
	config, configErr := kiroConfigForRegion(manager.config, options.Region)
	if configErr != nil {
		return polledDeviceAuthorization{}, configErr
	}
	startURL := kiroStartURL
	if options.Method == "iam_sso" {
		startURL = options.StartURL
	}
	base := strings.TrimRight(config.Issuer, "/")
	body, status, err := kiroJSON(ctx, config, base+"/client/register", map[string]any{
		"clientName": "Kiro", "clientType": "public", "issuerUrl": startURL,
		"grantTypes": []string{standardDeviceGrantType, "refresh_token"},
		"scopes":     kiroScopes,
	})
	if err != nil || status != http.StatusOK {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	registration, err := kiroRegistration(body, AccountTokens{KiroAuthMethod: options.Method, Region: options.Region}, config.Now())
	if err != nil {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	body, status, err = kiroJSON(ctx, config, base+"/device_authorization", map[string]string{
		"clientId": registration.OAuthClientID, "clientSecret": registration.OAuthClientSecret, "startUrl": startURL,
	})
	if err != nil || status != http.StatusOK {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	var device struct {
		DeviceCode           string `json:"deviceCode"`
		UserCode             string `json:"userCode"`
		VerificationURI      string `json:"verificationUri"`
		VerificationComplete string `json:"verificationUriComplete"`
		Interval             int64  `json:"interval"`
		ExpiresIn            int64  `json:"expiresIn"`
	}
	if json.Unmarshal(body, &device) != nil || device.DeviceCode == "" || device.UserCode == "" || device.ExpiresIn <= 0 || device.ExpiresIn > 3600 {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	verification := device.VerificationComplete
	if verification == "" {
		verification = device.VerificationURI
	}
	u, err := url.Parse(verification)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	if device.Interval > 3600 {
		return polledDeviceAuthorization{}, ErrDeviceCodeRequestFailed
	}
	result := polledDeviceAuthorization{
		kiroTokens:      registration,
		VerificationURL: verification, UserCode: device.UserCode, DeviceCode: device.DeviceCode,
		PollInterval: manager.clampDevicePollInterval(time.Duration(device.Interval) * time.Second),
		ExpiresIn:    time.Duration(device.ExpiresIn) * time.Second,
	}
	return result, nil
}

func (manager *SessionManager) pollKiroDeviceAuthorizationOnce(ctx context.Context, device polledDeviceAuthorization) (AccountTokens, bool, bool, error) {
	current := device.kiroTokens
	config, err := kiroConfigForRegion(manager.config, current.Region)
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	body, status, err := kiroJSON(ctx, config, config.TokenURL, map[string]string{
		"clientId": current.OAuthClientID, "clientSecret": current.OAuthClientSecret,
		"grantType": standardDeviceGrantType, "deviceCode": device.DeviceCode,
	})
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	if status == http.StatusBadRequest {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		if failure.Error == "authorization_pending" {
			return AccountTokens{}, true, false, nil
		}
		if failure.Error == "slow_down" {
			return AccountTokens{}, true, true, nil
		}
	}
	tokens, err := decodeKiroTokens(body, status, current, manager.config.Now())
	if err == nil && current.KiroAuthMethod == "iam_sso" && tokens.ProjectID == "" {
		err = resolveKiroProfile(ctx, config, &tokens)
	}
	return tokens, false, false, err
}

func (client *TokenClient) refreshKiro(ctx context.Context, current AccountTokens) (AccountTokens, error) {
	switch current.KiroAuthMethod {
	case "builder_id", "social", "iam_sso":
	default:
		return AccountTokens{}, ErrInvalidGrant
	}
	config, err := kiroConfigForRegion(client.config, current.Region)
	if err != nil {
		return AccountTokens{}, ErrInvalidGrant
	}
	if current.KiroAuthMethod == "social" {
		if current.RefreshToken == "" {
			return AccountTokens{}, ErrInvalidGrant
		}
		body, status, err := kiroJSON(ctx, config, kiroSocialBase(config)+"/refreshToken", map[string]string{"refreshToken": current.RefreshToken})
		if err != nil {
			return AccountTokens{}, err
		}
		return decodeKiroTokens(body, status, current, config.Now())
	}
	if current.OAuthClientID == "" || current.OAuthClientSecret == "" || current.RefreshToken == "" ||
		(!current.OAuthClientExpiresAt.IsZero() && !current.OAuthClientExpiresAt.After(client.config.Now())) {
		return AccountTokens{}, ErrInvalidGrant
	}
	body, status, err := kiroJSON(ctx, config, config.TokenURL, map[string]string{
		"clientId": current.OAuthClientID, "clientSecret": current.OAuthClientSecret,
		"grantType": "refresh_token", "refreshToken": current.RefreshToken,
	})
	if err != nil {
		return AccountTokens{}, err
	}
	return decodeKiroTokens(body, status, current, client.config.Now())
}

func decodeKiroTokens(body []byte, status int, current AccountTokens, now time.Time) (AccountTokens, error) {
	if status != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &failure)
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			switch failure.Error {
			case "invalid_grant", "invalid_client", "unauthorized_client", "expired_token", "access_denied":
				return AccountTokens{}, ErrInvalidGrant
			}
		}
		return AccountTokens{}, fmt.Errorf("Kiro token request returned status %d", status)
	}
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileARN   string `json:"profileArn"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if json.Unmarshal(body, &result) != nil || strings.TrimSpace(result.AccessToken) == "" || result.ExpiresIn <= 0 || result.ExpiresIn > 7*24*3600 {
		return AccountTokens{}, fmt.Errorf("invalid Kiro token response")
	}
	current.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		current.RefreshToken = result.RefreshToken
	}
	if result.ProfileARN != "" {
		current.ProjectID = result.ProfileARN
	}
	current.TokenType = "Bearer"
	current.ExpiresAt = now.Add(time.Duration(result.ExpiresIn) * time.Second).UTC()
	if err := current.Validate(); err != nil {
		return AccountTokens{}, fmt.Errorf("incomplete Kiro token response")
	}
	return current, nil
}
