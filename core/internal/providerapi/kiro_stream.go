package providerapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const kiroMaxFrame = 16 << 20
const kiroMaxResponse = 64 << 20

// KiroResponse lazily translates AWS event-stream frames, preserving backpressure
// and cancellation through the original body. No reader goroutine outlives it.
func KiroResponse(res *http.Response, exchange *KiroExchange) error {
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil
	}
	if !strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "application/vnd.amazon.eventstream") {
		return fmt.Errorf("Kiro returned an unexpected response content type")
	}
	reader := &kiroStream{source: res.Body, exchange: exchange, id: randomUUID(), textIndex: -1, toolIndex: make(map[string]int)}
	res.Header.Del("Content-Length")
	res.Header.Del("Content-Encoding")
	res.ContentLength = -1
	if exchange.Streaming {
		res.Header.Set("Content-Type", "text/event-stream")
		res.Body = reader
		return nil
	}
	defer reader.Close()
	for !reader.done {
		if err := reader.next(); err != nil {
			return err
		}
	}
	body, err := json.Marshal(reader.message())
	if err != nil {
		return err
	}
	res.Header.Set("Content-Type", "application/json")
	res.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

type kiroBlock struct {
	kind, id, name string
	text           strings.Builder
	input          strings.Builder
	closed         bool
}

type kiroStream struct {
	source                                           io.ReadCloser
	exchange                                         *KiroExchange
	id                                               string
	pending                                          []byte
	blocks                                           []*kiroBlock
	toolIndex                                        map[string]int
	lastTool                                         string
	textIndex                                        int
	started, done, terminal, output                  bool
	stopReason                                       string
	inputTokens, outputTokens, cacheRead, cacheWrite int64
	inputKnown                                       bool
	uncachedInput                                    int64
	bytes                                            int
	failure                                          error
}

func (r *kiroStream) Close() error { return r.source.Close() }
func (r *kiroStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if r.failure != nil {
			return 0, r.failure
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.failure = err
			// A partial stream ends with an explicit protocol error, never a
			// success stop. The following read still reports the I/O failure.
			if r.started {
				r.emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "Kiro response stream was incomplete or invalid"}})
			} else {
				return 0, err
			}
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *kiroStream) emit(event string, value any) {
	if !r.exchange.Streaming {
		return
	}
	raw, _ := json.Marshal(value)
	r.pending = append(r.pending, "event: "+event+"\ndata: "...)
	r.pending = append(r.pending, raw...)
	r.pending = append(r.pending, '\n', '\n')
}

func (r *kiroStream) start() {
	if r.started {
		return
	}
	r.started = true
	r.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": r.id, "type": "message", "role": "assistant", "model": r.exchange.Model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (r *kiroStream) next() error {
	headers, raw, err := readKiroFrame(r.source)
	if err == io.EOF {
		return r.finish()
	}
	if err != nil {
		return err
	}
	r.bytes += len(raw)
	if r.bytes > kiroMaxResponse {
		return fmt.Errorf("Kiro response exceeds size limit")
	}
	if headers[":message-type"] == "exception" || headers[":message-type"] == "error" {
		return kiroStreamError(headers, gjson.ParseBytes(raw))
	}
	if !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return fmt.Errorf("invalid Kiro event payload")
	}
	event := gjson.ParseBytes(raw)
	r.readUsage(event, 0)
	if reason := event.Get("stopReason").String(); reason != "" {
		r.terminal = true
		switch reason {
		case "max_tokens", "maxTokens", "MAX_TOKENS", "length":
			r.stopReason = "max_tokens"
		case "tool_use", "toolUse", "TOOL_USE":
			r.stopReason = "tool_use"
		default:
			r.stopReason = "end_turn"
		}
	}
	switch headers[":event-type"] {
	case "assistantResponseEvent":
		return r.text("text", event.Get("content").String())
	case "reasoningContentEvent":
		return r.text("thinking", event.Get("text").String())
	case "toolUseEvent":
		return r.tool(event)
	case "meteringEvent":
		r.terminal = true
	}
	return nil
}

func (r *kiroStream) text(kind, text string) error {
	if text == "" {
		return nil
	}
	r.start()
	r.output = true
	if r.textIndex < 0 || r.blocks[r.textIndex].kind != kind || r.blocks[r.textIndex].closed {
		r.closeText()
		r.textIndex = len(r.blocks)
		r.blocks = append(r.blocks, &kiroBlock{kind: kind})
		block := map[string]any{"type": kind}
		if kind == "thinking" {
			block["thinking"] = ""
		} else {
			block["text"] = ""
		}
		r.emit("content_block_start", map[string]any{"type": "content_block_start", "index": r.textIndex, "content_block": block})
	}
	if !r.exchange.Streaming {
		r.blocks[r.textIndex].text.WriteString(text)
	}
	delta := map[string]string{"type": kind + "_delta"}
	if kind == "thinking" {
		delta["thinking"] = text
	} else {
		delta["text"] = text
	}
	r.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": r.textIndex, "delta": delta})
	return nil
}

func (r *kiroStream) closeBlock(index int) {
	if r.blocks[index].closed {
		return
	}
	r.blocks[index].closed = true
	r.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}
func (r *kiroStream) closeText() {
	if r.textIndex >= 0 {
		r.closeBlock(r.textIndex)
		r.textIndex = -1
	}
}

func (r *kiroStream) tool(event gjson.Result) error {
	id, name := event.Get("toolUseId").String(), event.Get("name").String()
	if id == "" {
		id = r.lastTool
	}
	index, exists := r.toolIndex[id]
	if !exists {
		if id == "" || name == "" {
			return fmt.Errorf("Kiro tool event omitted its identity")
		}
		r.start()
		r.closeText()
		r.output = true
		index = len(r.blocks)
		r.toolIndex[id] = index
		r.blocks = append(r.blocks, &kiroBlock{kind: "tool_use", id: id, name: name})
		r.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
	}
	block := r.blocks[index]
	if block.closed || (name != "" && name != block.name) {
		return fmt.Errorf("conflicting Kiro tool event")
	}
	r.lastTool = id
	input := event.Get("input")
	fragment := ""
	if input.Type == gjson.String {
		fragment = input.String()
	} else if input.IsObject() {
		fragment = input.Raw
	} else if input.Exists() && input.Type != gjson.Null {
		return fmt.Errorf("invalid Kiro tool input")
	}
	if fragment != "" {
		block.input.WriteString(fragment)
		r.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "input_json_delta", "partial_json": fragment}})
	}
	if event.Get("stop").Bool() {
		if err := validateKiroTool(block); err != nil {
			return err
		}
		r.closeBlock(index)
	}
	return nil
}

func validateKiroTool(block *kiroBlock) error {
	input := block.input.String()
	if input == "" {
		block.input.WriteString("{}")
		return nil
	}
	if !gjson.Valid(input) || !gjson.Parse(input).IsObject() {
		return fmt.Errorf("incomplete Kiro tool arguments")
	}
	return nil
}

func (r *kiroStream) finish() error {
	if !r.terminal || !r.output {
		return io.ErrUnexpectedEOF
	}
	for i, block := range r.blocks {
		if block.kind == "tool_use" {
			if err := validateKiroTool(block); err != nil {
				return err
			}
		}
		r.closeBlock(i)
	}
	if r.stopReason == "" {
		r.stopReason = "end_turn"
		if len(r.toolIndex) > 0 {
			r.stopReason = "tool_use"
		}
	}
	r.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": r.stopReason, "stop_sequence": nil}, "usage": r.usage()})
	r.emit("message_stop", map[string]string{"type": "message_stop"})
	r.done = true
	return nil
}

func (r *kiroStream) usage() map[string]int64 {
	total := r.inputTokens
	if !r.inputKnown {
		total = r.uncachedInput + r.cacheRead + r.cacheWrite
	}
	read := min(r.cacheRead, total)
	write := min(r.cacheWrite, total-read)
	// Kiro's input total includes cached tokens. Messages reports only the
	// uncached portion in input_tokens, with cache counts alongside it.
	return map[string]int64{"input_tokens": total - read - write, "output_tokens": r.outputTokens,
		"cache_read_input_tokens": read, "cache_creation_input_tokens": write}
}
func (r *kiroStream) readUsage(event gjson.Result, depth int) {
	if depth > 5 {
		return
	}
	for _, field := range []struct {
		names  []string
		target *int64
	}{
		{[]string{"inputTokens", "input_tokens", "promptTokens", "prompt_tokens"}, &r.inputTokens},
		{[]string{"outputTokens", "output_tokens", "completionTokens", "completion_tokens"}, &r.outputTokens},
		{[]string{"cacheReadInputTokens", "cache_read_input_tokens"}, &r.cacheRead},
		{[]string{"cacheWriteInputTokens", "cacheCreationInputTokens", "cache_creation_input_tokens"}, &r.cacheWrite},
	} {
		for _, name := range field.names {
			if value := event.Get(name); value.Type == gjson.Number && value.Float() >= 0 && value.Float() <= 1e12 && value.Float() == float64(value.Int()) {
				*field.target = value.Int()
				if field.target == &r.inputTokens {
					r.inputKnown = true
				}
				break
			}
		}
	}
	for _, name := range []string{"uncachedInputTokens", "uncached_input_tokens"} {
		if value := event.Get(name); value.Type == gjson.Number && value.Float() >= 0 && value.Float() <= 1e12 && value.Float() == float64(value.Int()) {
			r.uncachedInput = value.Int()
			break
		}
	}
	for _, key := range []string{"usage", "tokenUsage", "metadata", "metrics", "tokenUsageEvent"} {
		if child := event.Get(key); child.IsObject() {
			r.readUsage(child, depth+1)
		}
	}
}

func (r *kiroStream) message() map[string]any {
	content := make([]any, 0, len(r.blocks))
	for _, block := range r.blocks {
		switch block.kind {
		case "tool_use":
			content = append(content, map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": json.RawMessage(block.input.String())})
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": block.text.String()})
		default:
			content = append(content, map[string]any{"type": "text", "text": block.text.String()})
		}
	}
	return map[string]any{"id": r.id, "type": "message", "role": "assistant", "model": r.exchange.Model, "content": content, "stop_reason": r.stopReason, "stop_sequence": nil, "usage": r.usage()}
}

func readKiroFrame(reader io.Reader) (map[string]string, []byte, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(reader, prelude[:]); err != nil {
		return nil, nil, err
	}
	total, headerSize := binary.BigEndian.Uint32(prelude[:4]), binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > kiroMaxFrame || headerSize > total-16 || binary.BigEndian.Uint32(prelude[8:]) != crc32.ChecksumIEEE(prelude[:8]) {
		return nil, nil, fmt.Errorf("invalid Kiro event-stream prelude")
	}
	frame := make([]byte, int(total)-12)
	if _, err := io.ReadFull(reader, frame); err != nil {
		return nil, nil, err
	}
	crc := crc32.NewIEEE()
	_, _ = crc.Write(prelude[:])
	_, _ = crc.Write(frame[:len(frame)-4])
	if binary.BigEndian.Uint32(frame[len(frame)-4:]) != crc.Sum32() {
		return nil, nil, fmt.Errorf("invalid Kiro event-stream checksum")
	}
	headers := map[string]string{}
	for rest := frame[:headerSize]; len(rest) > 0; {
		nameSize := int(rest[0])
		rest = rest[1:]
		if nameSize == 0 || len(rest) < nameSize+1 {
			return nil, nil, fmt.Errorf("invalid Kiro event header")
		}
		name, kind := string(rest[:nameSize]), rest[nameSize]
		rest = rest[nameSize+1:]
		size := 0
		switch kind {
		case 0, 1:
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7:
			if len(rest) < 2 {
				return nil, nil, fmt.Errorf("invalid Kiro event header length")
			}
			size = int(binary.BigEndian.Uint16(rest[:2]))
			rest = rest[2:]
		default:
			return nil, nil, fmt.Errorf("unsupported Kiro event header type")
		}
		if len(rest) < size {
			return nil, nil, fmt.Errorf("truncated Kiro event header")
		}
		if kind == 7 {
			headers[name] = string(rest[:size])
		}
		rest = rest[size:]
	}
	return headers, frame[headerSize : len(frame)-4], nil
}
