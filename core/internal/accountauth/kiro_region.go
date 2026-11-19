package accountauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func kiroConfigForRegion(config OAuthConfig, region string) (OAuthConfig, error) {
	if !contract.ValidKiroRegion(region) {
		return config, fmt.Errorf("invalid Kiro authentication region")
	}
	if config.Issuer == DefaultKiroIssuer {
		config.Issuer = "https://oidc." + region + ".amazonaws.com"
	}
	if config.TokenURL == DefaultKiroIssuer+"/token" {
		config.TokenURL = strings.TrimRight(config.Issuer, "/") + "/token"
	}
	return config, nil
}

// OIDC and service regions are distinct: only a validated profile ARN selects
// another data plane. No credential is sent to an arbitrary imported hostname.
func KiroAPIBaseURL(configured string, tokens AccountTokens) string {
	return kiroRegionalURL(configured, "runtime", providerapi.KiroProfileRegion(tokens.ProjectID))
}

func KiroManagementURL(configured string, tokens AccountTokens) string {
	return kiroRegionalURL(configured, "management", providerapi.KiroProfileRegion(tokens.ProjectID))
}

func kiroRegionalURL(configured, plane, region string) string {
	base := strings.TrimRight(configured, "/")
	if base != DefaultKiroAPIBaseURL {
		return base
	}
	if region == "" {
		region = "us-east-1"
	}
	return "https://" + plane + "." + region + ".kiro.dev"
}

// EnsureKiroProfile resolves older grants whose access token response omitted the profile.
func EnsureKiroProfile(ctx context.Context, config OAuthConfig, tokens *AccountTokens) error {
	if tokens.ProjectID != "" {
		return nil
	}
	return resolveKiroProfile(ctx, config, tokens)
}

func resolveKiroProfile(ctx context.Context, config OAuthConfig, tokens *AccountTokens) error {
	regions := []string{tokens.Region, "us-east-1", "eu-central-1"}
	visited := map[string]bool{}
	client := *config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, region := range regions {
		if visited[region] || !contract.ValidKiroRegion(region) {
			continue
		}
		visited[region] = true
		base := kiroRegionalURL(config.APIBaseURL, "management", region)
		next := ""
		seen := map[string]bool{}
		for page := 0; page < 20; page++ {
			payload := map[string]string{}
			if next != "" {
				payload["nextToken"] = next
			}
			raw, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", bytes.NewReader(raw))
			if err != nil {
				return err
			}
			ApplyKiroAPIHeaders(req.Header, *tokens, config.Identities.KiroIdentityFor(ctx))
			req.Header.Del(providerapi.KiroProfileHeader)
			req.Header.Set("Content-Type", "application/x-amz-json-1.0")
			req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
			res, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				break
			}
			body, readErr := transport.ReadResponseBody(res, 1<<20)
			_ = res.Body.Close()
			if readErr != nil || res.StatusCode != 200 {
				break
			}
			var list struct {
				Profiles []struct {
					ARN string `json:"arn"`
				} `json:"profiles"`
				Next string `json:"nextToken"`
			}
			if json.Unmarshal(body, &list) != nil {
				break
			}
			for _, profile := range list.Profiles {
				if providerapi.KiroProfileRegion(profile.ARN) != "" {
					tokens.ProjectID = profile.ARN
					return nil
				}
			}
			if list.Next == "" || seen[list.Next] {
				break
			}
			next = list.Next
			seen[next] = true
		}
		// A test/internal API override is one endpoint, not several AWS regions.
		if config.APIBaseURL != DefaultKiroAPIBaseURL {
			break
		}
	}
	return fmt.Errorf("no usable Kiro profile is available for this account")
}
