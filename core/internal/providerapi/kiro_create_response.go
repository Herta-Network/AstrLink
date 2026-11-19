package providerapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Input items, call IDs, tools and reasoning stay in the Responses vocabulary.
// Apply the Runtime option contract before constructing Kiro's envelope.
func kiroCreateResponse(req *http.Request, root ojson.Result, profile, scope string, runtime KiroRuntime) (*KiroExchange, error) {
	model := root.Get("model").String()
	ex := &KiroExchange{Responses: true, Streaming: root.Get("stream").Bool(), Model: model, replay: runtime.replay, diagnostics: kiroCacheKeys(root)}
	body, err := kiroResponsesOptions(root, ex)
	if err != nil {
		return nil, err
	}
	input := root.Get("input")
	anchor := input.String()
	if input.IsArray() {
		anchor = root.Get("input.0").Raw
	}
	instructions := root.Get("instructions")
	if instructions.Exists() && instructions.Type != ojson.Null && instructions.Type != ojson.String {
		return nil, kiroParameter("instructions", "expected a string")
	}
	session := root.Get("metadata.session_id").String()
	if session == "" {
		session = root.Get("metadata.kiro:conversation_id").String()
	}
	conversationID := kiroConversationID(session, scope, model+"\x00"+instructions.String()+"\x00"+anchor)
	if runtime.ChatCompletions && runtime.replay.cache != nil {
		ex.chatConversation = conversationID
	}
	items := make([]any, 0)
	switch {
	case input.Type == ojson.String:
		items = append(items, map[string]string{"role": "user", "content": input.String()})
	case input.IsArray():
		restored := make(map[*kiroChatReasoning]bool)
		wireSize := len(root.Raw)
		for _, item := range input.Array() {
			if !item.IsObject() {
				return nil, kiroParameter("input", "expected a Responses input item")
			}
			if item.Get("type").String() == "item_reference" {
				return nil, kiroParameter("input", "Kiro cannot resolve stored item_reference IDs; send the actual input and output items")
			}
			if item.Get("type").String() == "reasoning" {
				if opaque := item.Get("encrypted_content").String(); opaque != "" && !runtime.replay.responsesItem(model, opaque, false) {
					return nil, ErrKiroThinkingUnsupported
				}
			}
			for _, field := range []string{"content", "output"} {
				for _, part := range item.Get(field).Array() {
					if part.Get("type").String() == "input_file" {
						if _, err := kiroDocumentBlock(part); err != nil {
							return nil, err
						}
					}
				}
			}
			if group := ex.chatReasoning(item); group != nil && !restored[group] {
				if wireSize+group.size > kiroMaxBody {
					return nil, kiroParameter("messages", "conversation with restored tool reasoning exceeds Kiro's request size limit")
				}
				for _, reasoning := range group.items {
					items = append(items, reasoning)
				}
				restored[group] = true
				wireSize += group.size
			}
			items = append(items, json.RawMessage(item.Raw))
		}
	default:
		return nil, kiroParameter("input", "Kiro Responses requires a string or item array with the conversation history")
	}
	if !runtime.SystemField {
		delete(body, "instructions")
		if prompt := instructions.String(); prompt != "" {
			items = append([]any{map[string]string{"role": "user", "content": prompt}, map[string]string{"role": "assistant", "content": "I will follow these instructions."}}, items...)
		}
	}
	body["input"] = items
	meta := map[string]any{}
	if value := root.Get("metadata"); value.Exists() && value.Type != ojson.Null {
		if !value.IsObject() {
			return nil, kiroParameter("metadata", "expected an object")
		}
		for key, val := range value.Map() {
			if !strings.HasPrefix(key, "kiro:") && key != "session_id" {
				meta[key] = json.RawMessage(val.Raw)
			}
		}
	}
	meta["kiro:conversation_id"] = conversationID
	meta["kiro:agent_continuation_id"] = randomUUID()
	meta["kiro:agent_mode"] = "vibe"
	body["metadata"] = meta
	body["origin"] = "AI_EDITOR"
	body["stream"] = true
	if profile != "" {
		body["profileArn"] = profile
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	setKiroBody(req, profile, true, encoded)
	return ex, nil
}
