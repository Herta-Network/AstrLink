package subscription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

type kiroCache[T any] struct {
	mu      sync.Mutex
	entries map[[32]byte]*kiroCacheEntry[T]
}
type kiroCacheEntry[T any] struct {
	value T
	err   error
	until time.Time
	ready chan struct{}
}

// The key changes on account, grant, profile, client version or proxy changes.
// Neither a credential nor a request context is retained by the cache.
func kiroCacheKey(ctx context.Context, tokens accountauth.AccountTokens, version string) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{tokens.AccessToken, tokens.ProjectID, version, networkproxy.Binding(ctx)}, "\x00")))
}
func (c *kiroCache[T]) get(ctx context.Context, key [32]byte, load func() (T, error)) (T, error) {
	for {
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[[32]byte]*kiroCacheEntry[T])
		}
		if old := c.entries[key]; old != nil && (old.until.IsZero() || time.Now().Before(old.until)) {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				var zero T
				return zero, ctx.Err()
			case <-old.ready:
			}
			if ctx.Err() == nil && (errors.Is(old.err, context.Canceled) || errors.Is(old.err, context.DeadlineExceeded)) {
				continue
			}
			return old.value, old.err
		}
		// Evict completed entries only; active loads have waiters.
		if len(c.entries) >= 128 {
			var oldest time.Time
			var drop [32]byte
			for k, e := range c.entries {
				if !e.until.IsZero() && (oldest.IsZero() || e.until.Before(oldest)) {
					drop, oldest = k, e.until
				}
			}
			if oldest.IsZero() {
				// All slots are in flight. Do not retain one more entry or evict another account's waiter.
				c.mu.Unlock()
				return load()
			}
			delete(c.entries, drop)
		}
		e := &kiroCacheEntry[T]{ready: make(chan struct{})}
		c.entries[key] = e
		c.mu.Unlock()
		e.value, e.err = load()
		c.mu.Lock()
		if e.err != nil {
			delete(c.entries, key)
		} else {
			e.until = time.Now().Add(15 * time.Minute)
		}
		close(e.ready)
		c.mu.Unlock()
		return e.value, e.err
	}
}

type kiroFeatures struct {
	profile     string
	systemField bool
}

// KiroRuntime uses only the credential resolved for this attempt. The server
// controls the dedicated prompt field independently of the ingress protocol.
func (m *Manager) KiroRuntime(ctx context.Context, h http.Header, model string, protocol contract.ProtocolID) (providerapi.KiroRuntime, error) {
	token, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return providerapi.KiroRuntime{}, fmt.Errorf("missing Kiro grant")
	}
	tokens := accountauth.AccountTokens{AccessToken: token, ProjectID: h.Get(providerapi.KiroProfileHeader)}
	ident, ok := providerapi.RecognizeKiroIdentity(h)
	if !ok {
		ident = providerapi.KiroIdentityAt(m.kiroConfig.Identities.KiroIdentityFor(ctx).Version, true)
	}
	features, err := m.kiroFeatures.get(ctx, kiroCacheKey(ctx, tokens, ident.Version), func() (kiroFeatures, error) {
		if err := accountauth.EnsureKiroProfile(ctx, m.kiroConfig, &tokens); err != nil {
			return kiroFeatures{}, err
		}
		return m.fetchKiroFeatures(ctx, tokens, ident)
	})
	if err != nil {
		return providerapi.KiroRuntime{}, err
	}
	runtime := providerapi.KiroRuntime{Profile: features.profile, SystemField: features.systemField}
	if protocol == contract.ProtocolOpenAIResponses {
		return runtime, nil
	}
	tokens.ProjectID = features.profile
	catalog, err := m.kiroModelCatalog(ctx, tokens)
	if err != nil {
		return providerapi.KiroRuntime{}, err
	}
	selected := catalog[model]
	runtime.ModelSchema = selected.ModelSchema
	runtime.MaxOutputTokens = selected.TokenLimits.MaxOutputTokens
	runtime.PromptCaching = selected.PromptCaching
	return runtime, nil
}

func (m *Manager) fetchKiroFeatures(ctx context.Context, tokens accountauth.AccountTokens, identity providerapi.KiroIdentity) (kiroFeatures, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	header := make(http.Header)
	providerapi.ApplyKiroHeaders(header, tokens.AccessToken, providerapi.KiroIdentityAt(identity.Version, false))
	endpoint := accountauth.KiroAPIBaseURL(m.kiroConfig.APIBaseURL, tokens) + "/"
	raw, err := m.kiroRPC(ctx, endpoint, "AmazonCodeWhispererStreamingService.GetFeatureConfiguration", map[string]any{
		"origin": "AI_EDITOR", "profileArn": tokens.ProjectID, "version": identity.Version,
	}, header, 1<<20)
	if err != nil {
		return kiroFeatures{}, err
	}
	var result struct {
		Configuration map[string]any `json:"configuration"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Configuration == nil {
		return kiroFeatures{}, fmt.Errorf("invalid Kiro feature configuration")
	}
	// Configuration keys are digests of the catalog salt and feature name.
	key := sha256.Sum256([]byte("kiro-feature-config-key-salt-3e9b1d7asystem_field_injection"))
	return kiroFeatures{
		profile:     tokens.ProjectID,
		systemField: result.Configuration[hex.EncodeToString(key[:])] == true,
	}, nil
}
