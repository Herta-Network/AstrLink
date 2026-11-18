package subscription

import (
	"bytes"
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

// The management operations use AWS JSON 1.0. The profile and editor origin
// live in the JSON input; GetUsageLimits also carries its query parameters.
func (manager *Manager) kiroRead(ctx context.Context, tokens accountauth.AccountTokens, operation string, input map[string]any, query url.Values) ([]byte, error) {
	input["origin"] = "AI_EDITOR"
	input["profileArn"] = tokens.ProjectID
	endpoint := accountauth.KiroManagementURL(manager.kiroConfig.APIBaseURL, tokens) + "/"
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	header := make(http.Header)
	accountauth.ApplyKiroAPIHeaders(header, tokens, manager.kiroConfig.Identities.KiroIdentityFor(ctx))
	header.Del(providerapi.KiroProfileHeader)
	return manager.kiroRPC(ctx, endpoint, operation, input, header, 4<<20)
}

func (manager *Manager) kiroRPC(ctx context.Context, endpoint, operation string, input map[string]any, header http.Header, maxBytes int64) ([]byte, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", operation)
	client := *manager.kiroConfig.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Kiro %s request failed", operation)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Kiro %s returned status %d", operation, res.StatusCode)
	}
	return transport.ReadResponseBody(res, maxBytes)
}

type kiroCatalogModel struct {
	ID            string                        `json:"modelId"`
	ModelSchema   json.RawMessage               `json:"additionalModelRequestFieldsSchema"`
	PromptCaching providerapi.KiroPromptCaching `json:"promptCaching"`
}

func (manager *Manager) KiroModels(ctx context.Context, tokens accountauth.AccountTokens) ([]string, error) {
	models, err := manager.kiroModelCatalog(ctx, tokens)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (manager *Manager) kiroModelCatalog(ctx context.Context, tokens accountauth.AccountTokens) (map[string]kiroCatalogModel, error) {
	return manager.kiroCatalog.get(ctx, kiroCacheKey(ctx, tokens, ""), func() (map[string]kiroCatalogModel, error) {
		if err := accountauth.EnsureKiroProfile(ctx, manager.kiroConfig, &tokens); err != nil {
			return nil, err
		}
		return manager.fetchKiroModels(ctx, tokens)
	})
}

func (manager *Manager) fetchKiroModels(ctx context.Context, tokens accountauth.AccountTokens) (map[string]kiroCatalogModel, error) {
	cursors := map[string]bool{}
	models := map[string]kiroCatalogModel{}
	next := ""
	for page := 0; page < 40; page++ {
		input := map[string]any{}
		if next != "" {
			input["nextToken"] = next
		}
		body, err := manager.kiroRead(ctx, tokens, "KiroControlPlaneBearerService.ListAvailableModels", input, nil)
		if err != nil {
			return nil, err
		}
		var catalog struct {
			Models []kiroCatalogModel `json:"models"`
			Next   string             `json:"nextToken"`
		}
		if json.Unmarshal(body, &catalog) != nil || catalog.Models == nil {
			return nil, fmt.Errorf("invalid Kiro model catalog")
		}
		for _, model := range catalog.Models {
			if !validKiroCacheCount(model.PromptCaching.MaximumCacheCheckpointsPerRequest) ||
				!validKiroCacheCount(model.PromptCaching.MinimumTokensPerCacheCheckpoint) {
				return nil, fmt.Errorf("invalid Kiro prompt caching limits")
			}
			id := strings.TrimSpace(model.ID)
			if id == "" || len(id) > 256 {
				continue
			}
			if _, seen := models[id]; seen {
				continue
			}
			models[id] = model
			if len(models) > contract.MaxServiceModels {
				return nil, fmt.Errorf("Kiro model catalog exceeds size limit")
			}
		}
		if catalog.Next == "" {
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

// The model catalog declares both optional counts as Smithy integers.
func validKiroCacheCount(count *int) bool {
	return count == nil || (*count >= 0 && int64(*count) <= math.MaxInt32)
}

func (manager *Manager) kiroUsage(ctx context.Context, tokens accountauth.AccountTokens) (contract.SubscriptionUsage, error) {
	if err := accountauth.EnsureKiroProfile(ctx, manager.kiroConfig, &tokens); err != nil {
		return contract.SubscriptionUsage{}, fmt.Errorf("%w: %w", ErrUsageUnavailable, err)
	}
	body, err := manager.kiroRead(ctx, tokens, "AmazonCodeWhispererService.GetUsageLimits", map[string]any{"isEmailRequired": false}, url.Values{"origin": {"AI_EDITOR"}, "isEmailRequired": {"false"}})
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
