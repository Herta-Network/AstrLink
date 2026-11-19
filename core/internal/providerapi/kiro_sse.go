package providerapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// CreateResponse always streams. Streaming callers receive Responses events;
// for a JSON caller we collect the terminal response and its finalized items.
type kiroResponsesStream struct {
	source    io.ReadCloser
	lines     *bufio.Scanner
	exchange  *KiroExchange
	output    *kiroOutputCollector
	pending   bytes.Reader
	end       error
	bytesRead int
}

func kiroResponsesBody(res *http.Response, ex *KiroExchange) error {
	s := &kiroResponsesStream{source: res.Body, lines: bufio.NewScanner(res.Body), exchange: ex}
	s.lines.Buffer(make([]byte, 4096), 4<<20)
	if !ex.Streaming || ex.chatConversation != "" {
		s.output = &kiroOutputCollector{
			items:     make(map[int]map[string]json.RawMessage),
			positions: make(map[string]int), calls: make(map[string]int),
			parts: make(map[int]map[int]*kiroOutputPart), arguments: make(map[int]*strings.Builder),
		}
	}
	if ex.Streaming {
		res.Body = s
		return nil
	}
	defer s.Close()
	for {
		raw, err := s.nextEvent()
		if err != nil {
			return err
		}
		event := ojson.ParseBytes(raw)
		if s.end == nil {
			continue
		}
		if s.end != io.EOF {
			return s.end
		}
		response := event.Get("response")
		body := []byte(response.Raw)
		if len(s.output.items) > 0 {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(body, &object); err != nil {
				return err
			}
			object["output"], err = s.output.marshal(event.Get("type").String() == "response.incomplete" || response.Get("status").String() == "incomplete")
			if err != nil {
				return err
			}
			body, err = json.Marshal(object)
			if err != nil {
				return err
			}
		}
		if ex.chatConversation != "" && event.Get("type").Str == "response.completed" && response.Get("status").Str != "incomplete" {
			ex.rememberChatTurn(s.output)
		}
		res.Header.Set("Content-Type", "application/json")
		res.ContentLength = int64(len(body))
		res.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
}

// The terminal event may omit output already delivered by the stream. Keep
// native output items (including unknown fields) and their original indices.
type kiroOutputCollector struct {
	arguments map[int]*strings.Builder
	items     map[int]map[string]json.RawMessage
	positions map[string]int
	calls     map[string]int
	nextIndex int
	order     []int
	parts     map[int]map[int]*kiroOutputPart
}
type kiroOutputPart struct {
	kind        string
	text        strings.Builder
	annotations json.RawMessage
}

func (c *kiroOutputCollector) observe(e ojson.Result) error {
	kind := e.Get("type").String()
	switch kind {
	case "response.output_item.added", "response.output_item.done", "response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
	default:
		return nil
	}
	id := e.Get("item_id").String()
	if id == "" {
		id = e.Get("item.id").String()
	}
	index, seen := c.positions[id]
	if !seen {
		index, seen = c.calls[e.Get("item.call_id").Str]
	}
	if pos := e.Get("output_index"); pos.Exists() {
		index = int(pos.Int())
	} else if !seen && (kind == "response.output_item.added" || kind == "response.output_item.done") {
		index = c.nextIndex
	}
	if index < 0 || index > kiroMaxResponse {
		return fmt.Errorf("invalid Kiro output index")
	}
	c.nextIndex = max(c.nextIndex, index+1)
	if id != "" {
		c.positions[id] = index
	}
	if c.items[index] == nil && (kind == "response.function_call_arguments.delta" || kind == "response.function_call_arguments.done") {
		// A compact stream may send arguments before the terminal call item.
		c.items[index] = map[string]json.RawMessage{"type": json.RawMessage(`"function_call"`)}
		if id != "" {
			c.items[index]["id"], _ = json.Marshal(id)
		}
	}
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		item := e.Get("item")
		if !item.IsObject() {
			return fmt.Errorf("missing Kiro output item")
		}
		record := c.items[index]
		if record == nil {
			record = map[string]json.RawMessage{}
			c.items[index] = record
		}
		if call := item.Get("call_id").Str; call != "" {
			c.calls[call] = index
		}
		return json.Unmarshal([]byte(item.Raw), &record)
	case "response.function_call_arguments.delta":
		if c.arguments[index] == nil {
			c.arguments[index] = new(strings.Builder)
		}
		c.arguments[index].WriteString(e.Get("delta").String())
	case "response.function_call_arguments.done":
		if args := e.Get("arguments"); args.Type == ojson.String && args.Str != "" {
			delete(c.arguments, index)
			c.items[index]["arguments"] = json.RawMessage(args.Raw)
		}
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		if c.items[index] == nil {
			c.items[index] = map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"assistant"`)}
			if id != "" {
				c.items[index]["id"], _ = json.Marshal(id)
			}
		}
		partIndex := int(e.Get("content_index").Int())
		if c.parts[index] == nil {
			c.parts[index] = map[int]*kiroOutputPart{}
		}
		part := c.parts[index][partIndex]
		if part == nil {
			part = &kiroOutputPart{kind: "output_text"}
			c.parts[index][partIndex] = part
		}
		if strings.HasPrefix(kind, "response.refusal.") {
			part.kind = "refusal"
		}
		if strings.HasSuffix(kind, ".delta") {
			part.text.WriteString(e.Get("delta").String())
		} else {
			field := "text"
			if part.kind == "refusal" {
				field = "refusal"
			}
			if final := e.Get(field); final.Type == ojson.String {
				part.text.Reset()
				part.text.WriteString(final.String())
			}
			if annotations := e.Get("annotations"); annotations.IsArray() {
				part.annotations = json.RawMessage(annotations.Raw)
			}
		}
	}
	return nil
}

// Terminal output can be a subset of items already emitted. Match stable IDs
// before considering array offsets, which shift when the service omits an item.
func (c *kiroOutputCollector) mergeTerminal(output ojson.Result, replayOnly bool) error {
	// Keep both the order of the streamed items and the relative order of
	// the terminal items. Pending terminal-only items precede their next
	// known neighbour, or follow the remaining stream if there is none.
	stream := slices.Sorted(maps.Keys(c.items))
	var pending []int
	for pos, item := range output.Array() {
		if !item.IsObject() {
			return fmt.Errorf("invalid Kiro terminal output item")
		}
		if replayOnly && !kiroChatReplayItem(item) {
			continue
		}
		id, call := item.Get("id").Str, item.Get("call_id").Str
		index, known := c.positions[id]
		if !known && call != "" {
			index, known = c.calls[call]
		}
		if !known && len(stream) > 0 && pos == stream[0] {
			// Text deltas may carry only an index, without an item ID.
			prior := c.items[pos]
			known = ojson.ParseBytes(prior["type"]).Str == item.Get("type").Str &&
				(len(prior["id"]) == 0 || id == "") && (len(prior["call_id"]) == 0 || call == "")
			index = pos
		}
		if known {
			for len(stream) > 0 && stream[0] < index {
				c.order = append(c.order, stream[0])
				stream = stream[1:]
			}
			if len(stream) == 0 || stream[0] != index {
				return fmt.Errorf("conflicting Kiro terminal output order")
			}
			stream = stream[1:]
			c.order = append(c.order, pending...)
			pending = pending[:0]
			c.order = append(c.order, index)
		} else {
			index = c.nextIndex
			c.nextIndex++
			pending = append(pending, index)
			if len(stream) > 0 && pos < stream[0] {
				c.order = append(c.order, pending...)
				pending = pending[:0]
			}
		}
		fields := c.items[index]
		if fields == nil {
			fields = make(map[string]json.RawMessage)
			c.items[index] = fields
		}
		for field, value := range item.Map() {
			switch field {
			case "content", "arguments", "encrypted_content", "summary":
				if len(fields[field]) != 0 && (value.Type == ojson.Null || value.Raw == `""` || value.IsArray() && len(value.Array()) == 0) {
					continue
				}
			}
			fields[field] = json.RawMessage(value.Raw)
		}
	}
	c.order = append(c.order, stream...)
	c.order = append(c.order, pending...)
	return nil
}

func (c *kiroOutputCollector) marshal(incomplete bool) ([]byte, error) {
	out := make([]map[string]json.RawMessage, 0, len(c.items))
	for _, index := range c.order {
		item := c.items[index]
		if string(item["type"]) == `"function_call"` {
			args := ojson.ParseBytes(item["arguments"]).String()
			if delta := c.arguments[index]; args == "" && delta != nil {
				args = delta.String()
				item["arguments"], _ = json.Marshal(args)
			}
			if incomplete && string(item["status"]) != `"completed"` {
				item["status"] = json.RawMessage(`"incomplete"`)
			} else if !json.Valid([]byte(args)) {
				return nil, fmt.Errorf("incomplete Kiro function arguments")
			}
		}
		if len(c.parts[index]) > 0 && len(ojson.ParseBytes(item["content"]).Array()) == 0 {
			content := []map[string]any{}
			for _, n := range slices.Sorted(maps.Keys(c.parts[index])) {
				part := c.parts[index][n]
				value := map[string]any{"type": part.kind}
				if part.kind == "refusal" {
					value["refusal"] = part.text.String()
				} else {
					value["text"] = part.text.String()
					if part.annotations != nil {
						value["annotations"] = part.annotations
					}
				}
				content = append(content, value)
			}
			item["content"], _ = json.Marshal(content)
		}
		out = append(out, item)
	}
	return json.Marshal(out)
}

func (s *kiroResponsesStream) Close() error { return s.source.Close() }

func (s *kiroResponsesStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.pending.Len() == 0 {
		if s.end != nil {
			return 0, s.end
		}
		raw, err := s.nextEvent()
		if err != nil {
			return 0, err
		}
		var frame bytes.Buffer
		frame.WriteString("event: ")
		frame.WriteString(ojson.GetBytes(raw, "type").String())
		frame.WriteString("\ndata: ")
		if err := json.Compact(&frame, raw); err != nil {
			return 0, err
		}
		frame.WriteString("\n\n")
		s.pending.Reset(frame.Bytes())
	}
	return s.pending.Read(p)
}

// Some Kiro responses omit the blank separator between single-line JSON events.
func (s *kiroResponsesStream) nextEvent() ([]byte, error) {
	var data []byte
	for s.lines.Scan() {
		line := s.lines.Bytes()
		s.bytesRead += len(line) + 1
		if s.bytesRead > kiroMaxResponse {
			return nil, fmt.Errorf("Kiro response exceeds size limit")
		}
		if len(line) == 0 && len(data) != 0 {
			return nil, fmt.Errorf("invalid Kiro SSE event")
		}
		v, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		if len(data) == 0 && bytes.Equal(v, []byte("[DONE]")) {
			return nil, io.ErrUnexpectedEOF
		}
		data = append(data, v...)
		if len(data) > kiroMaxFrame {
			return nil, fmt.Errorf("Kiro SSE event exceeds size limit")
		}
		if !json.Valid(data) {
			data = append(data, '\n')
			continue
		}
		e := ojson.ParseBytes(data)
		kind := e.Get("type").String()
		if kind == "" || strings.ContainsAny(kind, "\r\n") {
			return nil, fmt.Errorf("invalid Kiro event type")
		}
		// Buffered clients need every delta. Streaming Chat only needs the
		// signed items and call identities for the next tool turn.
		if s.output != nil && (!s.exchange.Streaming || kiroChatReplayItem(e.Get("item"))) {
			if err := s.output.observe(e); err != nil {
				return nil, err
			}
		}
		switch kind {
		case "response.output_item.done":
			s.remember(e.Get("item"))
		case "response.completed", "response.incomplete", "response.failed", "error":
			r := e.Get("response")
			failed := kind == "response.failed" || kind == "error" || r.Get("status").String() == "failed"
			var c kiroTokenCounts
			u := r.Get("usage")
			readKiroCount(&c.input, u, "input_tokens")
			readKiroCount(&c.output, u, "output_tokens")
			readKiroCount(&c.total, u, "total_tokens")
			readKiroCount(&c.cacheRead, u, "input_tokens_details.cached_tokens")
			readKiroCount(&c.cacheWrite, u, "input_tokens_details.cache_write_tokens")
			s.exchange.publishUsage(c, failed)
			if failed {
				failure := r.Get("error")
				if !failure.IsObject() {
					failure = e.Get("error")
				}
				if !failure.IsObject() {
					failure = e
				}
				s.end = kiroResponsesError(failure)
			} else {
				if !r.IsObject() {
					return nil, fmt.Errorf("Kiro terminal event omitted its response")
				}
				if s.output != nil && (len(s.output.items) != 0 || s.exchange.chatConversation != "") {
					if err := s.output.mergeTerminal(r.Get("output"), s.exchange.Streaming); err != nil {
						return nil, err
					}
					if s.exchange.Streaming && s.exchange.chatConversation != "" && kind == "response.completed" && r.Get("status").Str != "incomplete" {
						s.exchange.rememberChatTurn(s.output)
					}
				}
				for _, item := range r.Get("output").Array() {
					s.remember(item)
				}
				s.end = io.EOF
			}
		}
		return data, nil
	}
	if err := s.lines.Err(); err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

func (s *kiroResponsesStream) remember(item ojson.Result) {
	if item.Get("type").String() == "reasoning" {
		if opaque := item.Get("encrypted_content").String(); opaque != "" {
			s.exchange.replay.responsesItem(s.exchange.Model, opaque, true)
		}
	}
}
