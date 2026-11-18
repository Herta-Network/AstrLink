package providerapi

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// KiroReplay remembers upstream-issued reasoning blocks. Signed/opaque history
// is exempt from text redaction; client input alone cannot establish provenance.
// Only fixed-size digests are retained. A miss uses the normal repair policy.
type KiroReplay struct {
	mu    sync.Mutex
	known map[[32]byte]struct{}
	order [][32]byte
	next  int
}

type kiroReplayScope struct {
	cache *KiroReplay
	scope [32]byte
}

func (runtime *KiroRuntime) BindReplay(cache *KiroReplay, service, profile, account, caller string) {
	b, _ := json.Marshal([]string{service, profile, account, caller})
	runtime.replay = kiroReplayScope{cache, sha256.Sum256(b)}
}
func (s kiroReplayScope) check(model string, b *kiroReasoning, remember bool) bool {
	if s.cache == nil {
		return false
	}
	raw, _ := json.Marshal(struct {
		Scope [32]byte
		Model string
		Block *kiroReasoning
	}{s.scope, model, b})
	key := sha256.Sum256(raw)
	c := s.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.known[key]; ok {
		return true
	}
	if !remember {
		return false
	}
	if c.known == nil {
		c.known = make(map[[32]byte]struct{})
	}
	if len(c.order) < 4096 {
		c.order = append(c.order, key)
	} else {
		delete(c.known, c.order[c.next])
		c.order[c.next] = key
		c.next = (c.next + 1) % len(c.order)
	}
	c.known[key] = struct{}{}
	return true
}
