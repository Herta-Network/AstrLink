package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

func (manager *Manager) kiroRead(ctx context.Context, tokens accountauth.AccountTokens, path string, query url.Values) ([]byte, error) {
	query.Set("origin", "AI_EDITOR")
	if tokens.ProjectID != "" {
		query.Set("profileArn", tokens.ProjectID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, accountauth.KiroAPIBaseURL(manager.kiroConfig.APIBaseURL, tokens)+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	accountauth.ApplyKiroAPIHeaders(req.Header, tokens, manager.kiroConfig.Identities.KiroIdentityFor(ctx))
	req.Header.Del(providerapi.KiroProfileHeader)
	req.Header.Set("Accept", "application/json")
	client := *manager.kiroConfig.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Kiro account request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Kiro account request returned status %d", res.StatusCode)
	}
	return transport.ReadResponseBody(res, 4<<20)
}

func (manager *Manager) KiroModels(ctx context.Context, tokens accountauth.AccountTokens) ([]string, error) {
	seen, cursors := map[string]bool{}, map[string]bool{}
	models := []string{}
	next := ""
	for page := 0; page < 40; page++ {
		query := url.Values{"maxResults": {"50"}}
		if next != "" {
			query.Set("nextToken", next)
		}
		body, err := manager.kiroRead(ctx, tokens, "/ListAvailableModels", query)
		if err != nil {
			return nil, err
		}
		var catalog struct {
			Models []struct {
				ID string `json:"modelId"`
			} `json:"models"`
			Next string `json:"nextToken"`
		}
		if json.Unmarshal(body, &catalog) != nil || catalog.Models == nil {
			return nil, fmt.Errorf("invalid Kiro model catalog")
		}
		for _, model := range catalog.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" || len(id) > 256 || seen[id] {
				continue
			}
			seen[id] = true
			models = append(models, id)
			if len(models) > contract.MaxServiceModels {
				return nil, fmt.Errorf("Kiro model catalog exceeds size limit")
			}
		}
		if catalog.Next == "" {
			sort.Strings(models)
			return models, nil
		}
		if cursors[catalog.Next] {
			return nil, fmt.Errorf("Kiro model catalog repeated a page")
		}
		cursors[catalog.Next] = true
		next = catalog.Next
	}
	return nil, fmt.Errorf("Kiro model catalog exceeds page limit")
}

func (manager *Manager) kiroUsage(ctx context.Context, tokens accountauth.AccountTokens) (contract.SubscriptionUsage, error) {
	body, err := manager.kiroRead(ctx, tokens, "/getUsageLimits", url.Values{"resourceType": {"AGENTIC_REQUEST"}, "isEmailRequired": {"false"}})
	if err != nil {
		return contract.SubscriptionUsage{}, fmt.Errorf("%w: %w", ErrUsageUnavailable, err)
	}
	return DecodeKiroUsage(body, manager.now().UTC())
}

type kiroAllowance struct {
	Used         float64  `json:"currentUsage"`
	UsedPrecise  *float64 `json:"currentUsageWithPrecision"`
	Limit        float64  `json:"usageLimit"`
	LimitPrecise *float64 `json:"usageLimitWithPrecision"`
}

func (a kiroAllowance) amounts() (float64, float64, error) {
	used, limit := a.Used, a.Limit
	if a.UsedPrecise != nil {
		used = *a.UsedPrecise
	}
	if a.LimitPrecise != nil {
		limit = *a.LimitPrecise
	}
	if math.IsNaN(used) || math.IsInf(used, 0) || math.IsNaN(limit) || math.IsInf(limit, 0) || used < 0 || limit < 0 {
		return 0, 0, fmt.Errorf("invalid Kiro allowance")
	}
	return used, limit, nil
}

func DecodeKiroUsage(body []byte, now time.Time) (contract.SubscriptionUsage, error) {
	var payload struct {
		Reset        float64 `json:"nextDateReset"`
		Subscription struct {
			Type string `json:"subscriptionType"`
		} `json:"subscriptionInfo"`
		Breakdown []struct {
			kiroAllowance
			Resource string `json:"resourceType"`
			Trial    *struct {
				kiroAllowance
				Status string  `json:"freeTrialStatus"`
				Expiry float64 `json:"freeTrialExpiry"`
			} `json:"freeTrialInfo"`
			Bonuses []struct {
				kiroAllowance
				Status string  `json:"status"`
				Expiry float64 `json:"expiresAt"`
			} `json:"bonuses"`
		} `json:"usageBreakdownList"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Breakdown == nil {
		return contract.SubscriptionUsage{}, fmt.Errorf("invalid Kiro usage response")
	}
	result := contract.SubscriptionUsage{PlanType: payload.Subscription.Type}
	if err := contract.ValidatePlanType(result.PlanType); err != nil {
		return contract.SubscriptionUsage{}, err
	}
	for _, b := range payload.Breakdown {
		if b.Resource != "CREDIT" && b.Resource != "AGENTIC_REQUEST" {
			continue
		}
		used, limit, err := b.amounts()
		if err != nil {
			return result, err
		}
		if b.Trial != nil && b.Trial.Status == "ACTIVE" && (b.Trial.Expiry == 0 || b.Trial.Expiry > float64(now.Unix())) {
			u, l, e := b.Trial.amounts()
			if e != nil {
				return result, e
			}
			used += u
			limit += l
		}
		for _, bonus := range b.Bonuses {
			if bonus.Status != "ACTIVE" || (bonus.Expiry > 0 && bonus.Expiry <= float64(now.Unix())) {
				continue
			}
			u, l, e := bonus.amounts()
			if e != nil {
				return result, e
			}
			used += u
			limit += l
		}
		if limit > 0 {
			result.Primary = &contract.RateLimitWindow{UsedPercent: math.Min(1000, 100*used/limit)}
			if payload.Reset > float64(now.Unix()) && payload.Reset < float64(now.AddDate(1, 0, 0).Unix()) {
				reset := time.Unix(int64(payload.Reset), 0).UTC()
				result.Primary.ResetAt = &reset
			}
		}
		return result, nil
	}
	return result, fmt.Errorf("Kiro usage response omitted its credit allowance")
}
