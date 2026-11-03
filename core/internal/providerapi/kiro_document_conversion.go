package providerapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/tidwall/gjson"
)

// KiroDocumentConversion keeps files outside RelayKit's narrower Anthropic
// document conversion. Only typed inline file blocks are lifted; tool schemas
// and arbitrary tool payload fields are never traversed.
type KiroDocumentConversion map[string]map[string]any

func PrepareKiroDocuments(protocol contract.ProtocolID, body []byte) ([]byte, KiroDocumentConversion, error) {
	if protocol != contract.ProtocolOpenAIChat && protocol != contract.ProtocolOpenAIResponses {
		return body, nil, nil
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return nil, nil, fmt.Errorf("invalid Kiro conversion input")
	}
	documents := KiroDocumentConversion{}
	lift := func(content any) error {
		blocks, _ := content.([]any)
		for i, entry := range blocks {
			block, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			kind, _ := block["type"].(string)
			if kind != "file" && kind != "input_file" {
				continue
			}
			raw, _ := json.Marshal(block)
			doc, err := kiroDocumentBlock(gjson.ParseBytes(raw))
			if err != nil {
				return err
			}
			mime := ""
			for media, format := range kiroDocumentFormats {
				if format == doc.Format {
					mime = media
					break
				}
			}
			marker := "__kiro_document_" + randomUUID() + "__"
			documents[marker] = map[string]any{"type": "document", "title": doc.Name, "source": map[string]any{"type": "base64", "media_type": mime, "data": doc.Source.Bytes}}
			textType := "text"
			if protocol == contract.ProtocolOpenAIResponses {
				textType = "input_text"
			}
			blocks[i] = map[string]any{"type": textType, "text": marker}
		}
		return nil
	}
	key := "messages"
	if protocol == contract.ProtocolOpenAIResponses {
		key = "input"
	}
	items, _ := root[key].([]any)
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if err := lift(item["content"]); err != nil {
			return nil, nil, err
		}
		if item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
			if err := lift(item["output"]); err != nil {
				return nil, nil, err
			}
		}
	}
	if len(documents) == 0 {
		return body, nil, nil
	}
	out, err := json.Marshal(root)
	return out, documents, err
}

func (documents KiroDocumentConversion) Restore(body []byte) ([]byte, error) {
	if len(documents) == 0 {
		return body, nil
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return nil, fmt.Errorf("invalid converted Kiro document body")
	}
	counts := map[string]int{}
	split := func(text string) []any {
		var result []any
		for text != "" {
			index, marker := -1, ""
			for candidate := range documents {
				if found := strings.Index(text, candidate); found >= 0 && (index < 0 || found < index) {
					index, marker = found, candidate
				}
			}
			if index < 0 {
				result = append(result, map[string]any{"type": "text", "text": text})
				break
			}
			if index > 0 {
				result = append(result, map[string]any{"type": "text", "text": text[:index]})
			}
			result = append(result, documents[marker])
			counts[marker]++
			text = text[index+len(marker):]
		}
		return result
	}
	var restore func(any) any
	restore = func(value any) any {
		if text, ok := value.(string); ok {
			for marker := range documents {
				if strings.Contains(text, marker) {
					return split(text)
				}
			}
			return value
		}
		blocks, ok := value.([]any)
		if !ok {
			return value
		}
		result := make([]any, 0, len(blocks))
		for _, entry := range blocks {
			block, ok := entry.(map[string]any)
			if !ok {
				result = append(result, entry)
				continue
			}
			if block["type"] == "text" {
				text, _ := block["text"].(string)
				changed := false
				for marker := range documents {
					changed = changed || strings.Contains(text, marker)
				}
				if changed {
					result = append(result, split(text)...)
					continue
				}
			}
			if block["type"] == "tool_result" {
				block["content"] = restore(block["content"])
			}
			result = append(result, block)
		}
		return result
	}
	messages, _ := root["messages"].([]any)
	for _, entry := range messages {
		if message, ok := entry.(map[string]any); ok {
			message["content"] = restore(message["content"])
		}
	}
	for marker := range documents {
		if counts[marker] != 1 {
			return nil, fmt.Errorf("protocol conversion did not preserve a Kiro document")
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	for marker := range documents {
		if bytes.Contains(out, []byte(marker)) {
			return nil, fmt.Errorf("unrestored Kiro document marker")
		}
	}
	return out, nil
}
