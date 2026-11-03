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
	if strings.TrimRight(configured, "/") != DefaultKiroAPIBaseURL {
		return strings.TrimRight(configured, "/")
	}
	region := providerapi.KiroProfileRegion(tokens.ProjectID)
	if region == "" || region == "us-east-1" {
		return DefaultKiroAPIBaseURL
	}
	return "https://q." + region + ".amazonaws.com"
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
		base := strings.TrimRight(config.APIBaseURL, "/")
		if base == DefaultKiroAPIBaseURL && region != "us-east-1" {
			base = "https://q." + region + ".amazonaws.com"
		}
		next := ""
		seen := map[string]bool{}
		for page := 0; page < 20; page++ {
			payload := map[string]string{}
			if next != "" {
				payload["nextToken"] = next
			}
			raw, _ := json.Marshal(payload)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/ListAvailableProfiles", bytes.NewReader(raw))
			if err != nil {
				return err
			}
			ApplyKiroAPIHeaders(req.Header, *tokens, config.Identities.KiroIdentityFor(ctx))
			req.Header.Del(providerapi.KiroProfileHeader)
			req.Header.Set("Content-Type", "application/json")
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
