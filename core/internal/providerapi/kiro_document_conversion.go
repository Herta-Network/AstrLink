package providerapi

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/MoeForge/moego/ojson"
)

// RelayKit converts message content parts to Responses parts, but serializes a
// multipart Chat tool result as a JSON string. Convert those parts as a marked
// user message and put them back on the original function call after conversion.
type KiroToolOutputs map[string]string

func PrepareKiroChatParts(body []byte) ([]byte, KiroToolOutputs, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, nil, err
	}
	if stop := root["stop"]; stop != nil {
		if list, ok := stop.([]any); !ok || len(list) != 0 {
			return nil, nil, kiroParameter("stop", "Kiro CreateResponse cannot enforce stop sequences")
		}
	}
	var outputs KiroToolOutputs
	changed := false
	messages, _ := root["messages"].([]any)
	seenTurn := false
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			raw, err := json.Marshal(message["content"])
			if err != nil {
				return nil, nil, err
			}
			text, err := kiroSystemMessageText(ojson.ParseBytes(raw))
			if err != nil {
				return nil, nil, kiroParameter("messages", "Kiro instructions require text content")
			}
			if seenTurn {
				// RelayKit hoists every instruction to the global prompt. Match
				// GAR's representation for an instruction introduced mid-turn.
				message["role"] = "user"
				message["content"] = kiroSystemReminder(text)
				changed = true
			}
			continue
		}
		seenTurn = true
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for i, part := range parts {
			block, _ := part.(map[string]any)
			if block["type"] != "file" && block["type"] != "input_file" {
				continue
			}
			// RelayKit expects Chat's nested file shape. Keep inline files
			// supplied in flat Responses or source form usable here too.
			if block["type"] == "file" && block["file"] != nil {
				continue
			}
			raw, err := json.Marshal(block)
			if err != nil {
				return nil, nil, err
			}
			file := ojson.ParseBytes(raw)
			doc, err := kiroDocumentBlock(file)
			if err != nil {
				return nil, nil, err
			}
			name := file.Get("filename").String()
			if name == "" {
				name = doc.Name + "." + doc.Format
			}
			data := file.Get("file_data").String()
			if data == "" {
				for mime, format := range kiroDocumentFormats {
					if format == doc.Format {
						data = "data:" + mime + ";base64," + doc.Source.Bytes
						break
					}
				}
			}
			parts[i] = map[string]any{"type": "file", "file": map[string]string{"filename": name, "file_data": data}}
			changed = true
		}
		if message["role"] != "tool" && message["role"] != "function" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		if id == "" {
			return nil, nil, fmt.Errorf("multipart Kiro tool result requires a tool_call_id")
		}
		marker := randomUUID()
		if outputs == nil {
			outputs = KiroToolOutputs{}
		}
		outputs[marker] = id
		message["role"] = "user"
		delete(message, "tool_call_id")
		message["content"] = append([]any{map[string]string{"type": "text", "text": marker}}, parts...)
		changed = true
	}
	if !changed {
		return body, nil, nil
	}
	out, err := json.Marshal(root)
	return out, outputs, err
}

func (outputs KiroToolOutputs) Restore(body []byte) ([]byte, error) {
	items := ojson.GetBytes(body, "input").Array()
	input := make([]json.RawMessage, 0, len(items))
	restored := 0
	for _, item := range items {
		parts := item.Get("content").Array()
		marker := item.Get("content.0.text").Str
		id, found := outputs[marker]
		if !found || item.Get("role").Str != "user" || len(parts) == 0 || parts[0].Get("type").Str != "input_text" {
			input = append(input, json.RawMessage(item.Raw))
			continue
		}
		content := make([]json.RawMessage, 0, len(parts)-1)
		for _, part := range parts[1:] {
			content = append(content, json.RawMessage(part.Raw))
		}
		out, err := json.Marshal(map[string]any{"type": "function_call_output", "call_id": id, "output": content})
		if err != nil {
			return nil, err
		}
		input = append(input, out)
		restored++
	}
	if restored != len(outputs) {
		return nil, fmt.Errorf("conversion lost Kiro tool result content")
	}
	return ojson.SetBytes(body, "input", input)
}
