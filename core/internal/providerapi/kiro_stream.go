package providerapi

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/QuantumNous/astrlink/core/contract"
)

const kiroMaxFrame = 16 << 20
const kiroMaxResponse = 64 << 20

// KiroResponse translates the selected Runtime protocol, preserving backpressure
// and cancellation through the original body. No reader goroutine outlives it.
func KiroResponse(res *http.Response, exchange *KiroExchange) error {
	if res.StatusCode == http.StatusTooManyRequests {
		ms, err := strconv.ParseFloat(strings.TrimSpace(res.Header.Get("x-amzn-kiro-ratelimit-retry-after")), 64)
		if err == nil && ms >= 0 && ms <= 1<<53 {
			res.Header.Set("Retry-After", strconv.FormatInt(int64(math.Ceil(ms/1000)), 10))
		}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil
	}
	want := "application/vnd.amazon.eventstream"
	if exchange.Responses {
		want = "text/event-stream"
	}
	media, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if media != want {
		return fmt.Errorf("Kiro returned an unexpected response content type")
	}

	res.Header.Del("Content-Length")
	res.Header.Del("Content-Encoding")
	res.ContentLength = -1
	if exchange.Responses {
		return kiroResponsesBody(res, exchange)
	}
	reader := &kiroStream{source: res.Body, exchange: exchange, id: randomUUID(), textIndex: -1, toolIndex: make(map[string]int)}
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
	kind, id, name, data string
	signature            strings.Builder
	text                 strings.Builder
	input                strings.Builder
	closed               bool
}

type kiroStream struct {
	source                          io.ReadCloser
	exchange                        *KiroExchange
	id                              string
	pending                         []byte
	blocks                          []*kiroBlock
	toolIndex                       map[string]int
	lastTool                        string
	textIndex                       int
	started, done, terminal, output bool
	stopReason                      string
	tokenCounts                     kiroTokenCounts
	bytes                           int
	failure                         error
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
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": r.usage(),
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
		return kiroStreamError(headers, ojson.ParseBytes(raw))
	}
	if !ojson.ValidBytes(raw) || !ojson.ParseBytes(raw).IsObject() {
		return fmt.Errorf("invalid Kiro event payload")
	}
	event := ojson.ParseBytes(raw)
	switch headers[":event-type"] {
	case "metadataEvent", "tokenUsageEvent":
		r.readUsage(event)
		if event.Get("stopDetails.refusal").IsObject() {
			r.exchange.RecordResponseLoss("kiro_refusal_details", "metadataEvent.stopDetails.refusal", "Messages carries the refusal stop reason but has no fields for Kiro's refusal details.")
		}
		if reason := event.Get("stopReason"); reason.Exists() {
			return r.setStop(reason.String())
		}
	case "codeReferenceEvent":
		if len(event.Get("references").Array()) > 0 {
			r.noteCodeReferences()
		}
	case "contextUsageEvent":
		if event.Get("contextUsagePercentage").Type == ojson.Number {
			r.noteContextPercentage()
		}
	case "error", "validationError", "serviceUnavailableError":
		return kiroStreamError(headers, event)
	case "assistantResponseEvent":
		return r.text("text", event.Get("content").String())
	case "reasoningContentEvent":
		if text := event.Get("text").String(); text != "" {
			r.text("thinking", text)
		}
		if sig := event.Get("signature").String(); sig != "" {
			r.signature(sig)
		}
		if data := event.Get("redactedContent").String(); data != "" {
			return r.redacted(data)
		}
		return nil
	case "toolUseEvent":
		return r.tool(event)
	case "meteringEvent":
		r.readUsage(event)
		if event.Get("usage").Type == ojson.Number {
			r.noteMetering()
		}
		r.terminal = true
	}
	return nil
}

func (r *kiroStream) noteCodeReferences() {
	r.exchange.RecordResponseLoss("kiro_code_references", "codeReferenceEvent.references", "Kiro repository, license and code-span annotations have no equivalent Messages citation.")
}
func (r *kiroStream) noteMetering() {
	r.exchange.RecordResponseLoss("kiro_metering_units", "meteringEvent.usage", "Kiro metering units are separate from token usage; Messages does not report these units.")
}
func (r *kiroStream) noteContextPercentage() {
	r.exchange.RecordResponseLoss("kiro_context_percentage", "contextUsageEvent.contextUsagePercentage", "Messages does not carry Kiro's context usage percentage.")
}

func (r *kiroStream) setStop(reason string) error {
	switch reason {
	case "MAX_TOKENS", "max_tokens", "maxTokens", "length":
		r.stopReason = "max_tokens"
	case "TOOL_USE", "tool_use", "toolUse":
		r.stopReason = "tool_use"
	case "END_TURN", "end_turn":
		r.stopReason = "end_turn"
	case "CONTENT_FILTERED":
		r.stopReason = "refusal"
	case "PAUSE_TURN":
		r.stopReason = "pause_turn"
	case "MODEL_CONTEXT_WINDOW_EXCEEDED":
		r.stopReason = "model_context_window_exceeded"
	case "COMPACTION_REQUIRED_FOR_MODEL_HANDOFF":
		return fmt.Errorf("Kiro requires context compaction for model handoff")
	default:
		return fmt.Errorf("unrecognized Kiro stop reason")
	}
	r.terminal = true
	return nil
}

func (r *kiroStream) text(kind, text string) error {
	if text == "" {
		return nil
	}
	if r.textIndex >= 0 && r.blocks[r.textIndex].signature.Len() > 0 {
		r.closeText()
	}
	r.openText(kind)
	if !r.exchange.Streaming || kind == "thinking" {
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

func (r *kiroStream) openText(kind string) {
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
}

func (r *kiroStream) signature(sig string) {
	r.openText("thinking")
	r.blocks[r.textIndex].signature.WriteString(sig)
	r.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": r.textIndex, "delta": map[string]string{"type": "signature_delta", "signature": sig}})
}

func (r *kiroStream) redacted(data string) error {
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return fmt.Errorf("invalid Kiro redacted reasoning")
	}
	r.start()
	r.closeText()
	index := len(r.blocks)
	r.output = true
	r.blocks = append(r.blocks, &kiroBlock{kind: "redacted_thinking", data: data})
	r.exchange.replay.check(r.exchange.Model, &kiroReasoning{Redacted: data}, true)
	r.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]string{"type": "redacted_thinking", "data": data}})
	r.closeBlock(index)
	return nil
}

func (r *kiroStream) closeBlock(index int) {
	if r.blocks[index].closed {
		return
	}
	block := r.blocks[index]
	if block.signature.Len() > 0 {
		r.exchange.replay.check(r.exchange.Model, &kiroReasoning{Text: &kiroReasoningText{Text: block.text.String(), Signature: block.signature.String()}}, true)
	}
	block.closed = true
	r.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}
func (r *kiroStream) closeText() {
	if r.textIndex >= 0 {
		r.closeBlock(r.textIndex)
		r.textIndex = -1
	}
}

func (r *kiroStream) tool(event ojson.Result) error {
	id, name := event.Get("toolUseId").String(), event.Get("name").String()
	input := event.Get("input")
	fragment := ""
	if input.Type == ojson.String {
		fragment = input.String()
	} else if input.IsObject() {
		fragment = input.Raw
	} else if input.Exists() && input.Type != ojson.Null {
		return fmt.Errorf("invalid Kiro tool input")
	}
	return r.toolPart(id, name, fragment, event.Get("stop").Bool())
}

func (r *kiroStream) toolPart(id, name, fragment string, stop bool) error {
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

	if fragment != "" {
		block.input.WriteString(fragment)
		r.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]string{"type": "input_json_delta", "partial_json": fragment}})
	}
	if stop {
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
	if !ojson.Valid(input) || !ojson.Parse(input).IsObject() {
		return fmt.Errorf("incomplete Kiro tool arguments")
	}
	return nil
}

func (r *kiroStream) finish() error {
	if !r.terminal || (!r.output && (r.stopReason == "" || r.stopReason == "end_turn" || r.stopReason == "tool_use")) {
		return io.ErrUnexpectedEOF
	}
	r.start()
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
	r.exchange.mu.Lock()
	if r.exchange.observedUsage != nil {
		r.exchange.observedUsage.BillingIncomplete = false
	}
	r.exchange.mu.Unlock()
	return nil
}

// The Smithy TokenUsage in metadataEvent has uncachedInputTokens and optional
// cacheReadInputTokens/cacheWriteInputTokens. totalTokens includes output, and
// normalizedTokenUsage is a scalar metric, never another token-count object.
type kiroTokenCounts struct {
	uncached, input, output, total, cacheRead, cacheWrite *int64
}

func (r *kiroStream) usage() map[string]int64 {
	c := r.tokenCounts
	usage := map[string]int64{}
	if c.output != nil {
		usage["output_tokens"] = *c.output
	}
	if c.cacheRead != nil {
		usage["cache_read_input_tokens"] = *c.cacheRead
	}
	if c.cacheWrite != nil {
		usage["cache_creation_input_tokens"] = *c.cacheWrite
	}
	if c.uncached != nil {
		usage["input_tokens"] = *c.uncached
	} else if c.input != nil {
		// Responses-style input already includes reported cache hits/writes.
		uncached := *c.input - usage["cache_read_input_tokens"] - usage["cache_creation_input_tokens"]
		if uncached >= 0 {
			usage["input_tokens"] = uncached
		}
	}
	return usage
}

func readKiroCount(target **int64, source ojson.Result, names ...string) {
	for _, name := range names {
		v := source.Get(name)
		if v.Type == ojson.Number && v.Float() >= 0 && v.Float() <= 1e12 && v.Float() == float64(v.Int()) {
			n := v.Int()
			*target = &n
			return
		}
	}
}

func (r *kiroStream) readUsage(event ojson.Result) {
	for _, source := range []ojson.Result{event, event.Get("usage"), event.Get("tokenUsage")} {
		if !source.IsObject() {
			continue
		}
		c := &r.tokenCounts
		readKiroCount(&c.input, source, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens")
		readKiroCount(&c.total, source, "totalTokens", "total_tokens")
		readKiroCount(&c.output, source, "outputTokens", "output_tokens", "completionTokens", "completion_tokens")
		readKiroCount(&c.uncached, source, "uncachedInputTokens", "uncached_input_tokens")
		readKiroCount(&c.cacheRead, source, "cacheReadInputTokens", "cache_read_input_tokens", "input_tokens_details.cached_tokens")
		readKiroCount(&c.cacheWrite, source, "cacheWriteInputTokens", "cacheCreationInputTokens", "cache_creation_input_tokens", "input_tokens_details.cache_write_tokens")
		if source.Get("normalizedTokenUsage").Type == ojson.Number {
			r.exchange.RecordResponseLoss("kiro_normalized_token_usage", "metadataEvent.tokenUsage.normalizedTokenUsage", "Messages does not carry Kiro's normalized token metric.")
		}
		if source.Get("contextUsagePercentage").Type == ojson.Number {
			r.noteContextPercentage()
		}
	}
	r.exchange.publishUsage(r.tokenCounts, true)
}

// ObservedUsage returns actual Kiro counts for the current upstream attempt,
// before cross-protocol renderers can insert placeholder counters.
func (e *KiroExchange) ObservedUsage() *contract.Usage {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.observedUsage == nil {
		return nil
	}
	u := *e.observedUsage
	return &u
}

func (e *KiroExchange) publishUsage(c kiroTokenCounts, partial bool) {
	if c.output == nil {
		return
	}
	var input int64
	switch {
	case c.input != nil:
		input = *c.input
	case c.total != nil && *c.total >= *c.output:
		input = *c.total - *c.output
	case c.uncached != nil && c.cacheRead != nil && c.cacheWrite != nil:
		input = *c.uncached + *c.cacheRead + *c.cacheWrite
	default:
		return
	}
	u := &contract.Usage{InputTokens: int(input), OutputTokens: int(*c.output), TotalTokens: int(input + *c.output), BillingIncomplete: partial}
	if c.cacheRead != nil {
		n := int(*c.cacheRead)
		u.CacheReadTokens = &n
	}
	if c.cacheWrite != nil {
		n := int(*c.cacheWrite)
		u.CacheWriteTokens = &n
	}
	e.mu.Lock()
	e.observedUsage = u
	e.mu.Unlock()
}

func (r *kiroStream) message() map[string]any {
	content := make([]any, 0, len(r.blocks))
	for _, block := range r.blocks {
		switch block.kind {
		case "tool_use":
			content = append(content, map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": json.RawMessage(block.input.String())})
		case "thinking":
			b := map[string]any{"type": "thinking", "thinking": block.text.String()}
			if block.signature.Len() > 0 {
				b["signature"] = block.signature.String()
			}
			content = append(content, b)
		case "redacted_thinking":
			content = append(content, map[string]any{"type": block.kind, "data": block.data})
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
