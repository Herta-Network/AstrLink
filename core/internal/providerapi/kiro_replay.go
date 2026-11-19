package providerapi

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"sync"

	"github.com/MoeForge/moego/ojson"
)

// KiroReplay remembers upstream-issued reasoning blocks. Signed/opaque history
// is exempt from text redaction; client input alone cannot establish provenance.
// Explicit replays use fixed-size digests. Chat needs a bounded, in-memory
// copy of upstream envelopes because its standard tool-call messages omit them.
type KiroReplay struct {
	mu        sync.Mutex
	known     map[[32]byte]struct{}
	order     [][32]byte
	next      int
	chat      map[[32]byte]*list.Element
	chatOrder list.List
	chatBytes int
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
	return s.digest(sha256.Sum256(raw), remember)
}

func (s kiroReplayScope) responsesItem(model, opaque string, remember bool) bool {
	if s.cache == nil {
		return false
	}
	// Canonical Kiro envelopes can also have been issued by the GAR encoder.
	// Never authenticate extra client-supplied fields by decoding them away.
	var block kiroReasoning
	if json.Unmarshal([]byte(opaque), &block) == nil && ((block.Text != nil && block.Text.Signature != "") != (block.Redacted != "")) {
		canonical, _ := json.Marshal(block)
		if string(canonical) == opaque {
			return s.check(model, &block, remember)
		}
	}
	raw, _ := json.Marshal([]any{s.scope, "responses", model, opaque})
	return s.digest(sha256.Sum256(raw), remember)
}

func (s kiroReplayScope) digest(key [32]byte, remember bool) bool {
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

// Each entry belongs to the service, grant, profile, caller, model and
// conversation that received the call. Store only the signed envelopes.
type kiroChatEntry struct {
	key   [32]byte
	group *kiroChatReasoning
}

type kiroChatReasoning struct {
	items []json.RawMessage
	size  int
}

func kiroChatReplayItem(item ojson.Result) bool {
	kind := item.Get("type").Str
	return kind == "reasoning" || kind == "function_call"
}

func (ex *KiroExchange) chatCallKey(id, name string) [32]byte {
	raw, _ := json.Marshal([]any{ex.replay.scope, ex.Model, ex.chatConversation, id, name})
	return sha256.Sum256(raw)
}

func (ex *KiroExchange) rememberChatTurn(output *kiroOutputCollector) {
	var group *kiroChatReasoning
	published := false
	for _, index := range output.order {
		item := output.items[index]
		switch ojson.ParseBytes(item["type"]).Str {
		case "reasoning":
			if ojson.ParseBytes(item["encrypted_content"]).Str == "" {
				continue
			}
			if published || group == nil {
				group = new(kiroChatReasoning)
				published = false
			}
			// Replay only the provider-issued envelope. Readable summaries
			// were already delivered to the Chat client.
			raw, err := json.Marshal(map[string]json.RawMessage{
				"type": json.RawMessage(`"reasoning"`), "summary": json.RawMessage(`[]`), "encrypted_content": item["encrypted_content"],
			})
			if err != nil {
				continue
			}
			group.items = append(group.items, raw)
			group.size += len(raw)
		case "function_call":
			id, name := ojson.ParseBytes(item["call_id"]).Str, ojson.ParseBytes(item["name"]).Str
			if id == "" {
				id = ojson.ParseBytes(item["id"]).Str
			}
			if group != nil && id != "" && name != "" {
				ex.replay.cache.storeChat(ex.chatCallKey(id, name), group)
				published = true
			}
		}
	}
}

func (c *KiroReplay) storeChat(key [32]byte, group *kiroChatReasoning) {
	const budget = 16 << 20
	if group.size > budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.chat == nil {
		c.chat = make(map[[32]byte]*list.Element)
	}
	if old := c.chat[key]; old != nil {
		c.chatBytes -= c.chatOrder.Remove(old).(*kiroChatEntry).group.size
	}
	c.chat[key] = c.chatOrder.PushBack(&kiroChatEntry{key: key, group: group})
	c.chatBytes += group.size
	for c.chatBytes > budget || c.chatOrder.Len() > 4096 {
		evicted := c.chatOrder.Remove(c.chatOrder.Front()).(*kiroChatEntry)
		delete(c.chat, evicted.key)
		c.chatBytes -= evicted.group.size
	}
}

func (ex *KiroExchange) chatReasoning(item ojson.Result) *kiroChatReasoning {
	if ex.chatConversation == "" || item.Get("type").Str != "function_call" {
		return nil
	}
	key := ex.chatCallKey(item.Get("call_id").Str, item.Get("name").Str)
	c := ex.replay.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.chat[key]; entry != nil {
		c.chatOrder.MoveToBack(entry)
		return entry.Value.(*kiroChatEntry).group
	}
	return nil
}
