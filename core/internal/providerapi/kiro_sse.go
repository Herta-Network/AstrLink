package providerapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// Each output index is stable within a response; function deltas use item IDs,
// while the next turn must carry the distinct call ID.
type kiroSSE struct {
	positions map[string]int
	lastIndex int
	nextIndex int
	lines     *bufio.Scanner
	calls     map[int]string
	reasoning map[int]*strings.Builder
	textSeen  map[[2]int]bool
	finished  map[int]bool
	size      int
}

func newKiroSSE(body io.Reader) *kiroSSE {
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 4096), 4<<20)
	return &kiroSSE{positions: map[string]int{}, lines: s, calls: map[int]string{}, reasoning: map[int]*strings.Builder{}, textSeen: map[[2]int]bool{}, finished: map[int]bool{}}
}

func (s *kiroSSE) next(r *kiroStream) error {
	var data strings.Builder
	for s.lines.Scan() {
		line := s.lines.Text()
		s.size += len(line) + 1
		if s.size > kiroMaxResponse {
			return fmt.Errorf("Kiro response exceeds size limit")
		}
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			raw := data.String()
			if !ojson.Valid(raw) {
				return fmt.Errorf("invalid Kiro SSE event")
			}
			return s.event(r, ojson.Parse(raw))
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			v = strings.TrimPrefix(v, " ")
			// Runtime emits a JSON object per data line. Some responses omit the
			// empty SSE separator; consume a complete object without waiting for EOF.
			if data.Len() == 0 && strings.TrimSpace(v) == "[DONE]" {
				continue
			}
			if data.Len() == 0 && ojson.Valid(v) {
				return s.event(r, ojson.Parse(v))
			}
			data.WriteString(v)
			data.WriteByte('\n')
		}
		if data.Len() > kiroMaxFrame {
			return fmt.Errorf("Kiro SSE event exceeds size limit")
		}
	}
	if err := s.lines.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (s *kiroSSE) event(r *kiroStream, e ojson.Result) error {
	kind := e.Get("type").String()
	if kind == "" {
		return fmt.Errorf("Kiro SSE event omitted its type")
	}
	if kind == "kiro:keep_alive" {
		r.emit("ping", map[string]string{"type": "ping"})
		return nil
	}
	index := s.lastIndex
	itemID := e.Get("item_id").String()
	if itemID == "" {
		itemID = e.Get("item.id").String()
	}
	if pos := e.Get("output_index"); pos.Exists() {
		index = int(pos.Int())
	} else if previous, ok := s.positions[itemID]; ok && itemID != "" {
		index = previous
	} else if kind == "response.output_item.added" || kind == "response.output_item.done" && s.finished[index] {
		index = s.nextIndex
	}
	if strings.HasPrefix(kind, "response.output_item.") || itemID != "" {
		if itemID != "" {
			s.positions[itemID] = index
		}
		s.lastIndex = index
		if index >= s.nextIndex {
			s.nextIndex = index + 1
		}
	}
	part := [2]int{index, int(e.Get("content_index").Int())}
	switch kind {
	case "kiro:code_reference.added":
		if len(e.Get("annotations").Array()) > 0 {
			r.noteCodeReferences()
		}
	case "response.output_text.delta", "response.refusal.delta":
		s.textSeen[part] = true
		return r.text("text", e.Get("delta").String())
	case "response.output_text.done", "response.refusal.done":
		if len(e.Get("annotations").Array()) > 0 {
			r.noteCodeReferences()
		}
		if !s.textSeen[part] {
			s.textSeen[part] = true
			text := e.Get("text").String()
			if kind == "response.refusal.done" {
				text = e.Get("refusal").String()
			}
			return r.text("text", text)
		}
	case "response.reasoning_text.delta", "response.reasoning.delta", "response.reasoning_summary_text.delta":
		if s.reasoning[index] == nil {
			s.reasoning[index] = &strings.Builder{}
		}
		s.reasoning[index].WriteString(e.Get("delta").String())
	case "response.output_item.added":
		item := e.Get("item")
		if item.Get("type").String() == "function_call" {
			return s.toolStart(r, index, item)
		}
	case "response.function_call_arguments.delta":
		id, ok := s.calls[index]
		if !ok {
			return fmt.Errorf("Kiro function delta has no call")
		}
		return r.toolPart(id, "", e.Get("delta").String(), false)
	case "response.function_call_arguments.done":
		return s.toolDone(r, index, e.Get("arguments").String())
	case "response.output_item.done":
		return s.itemDone(r, index, e.Get("item"))
	case "response.failed", "error":
		r.readResponsesUsage(e.Get("response.usage"))
		failure := e.Get("response.error")
		if !failure.Exists() {
			failure = e.Get("error")
		}
		if !failure.Exists() {
			failure = e
		}
		return kiroResponsesError(failure)
	case "response.completed", "response.incomplete":
		response := e.Get("response")
		if !response.IsObject() {
			return fmt.Errorf("Kiro terminal event omitted its response")
		}
		r.readResponsesUsage(response.Get("usage"))
		if response.Get("status").String() == "failed" {
			return kiroResponsesError(response.Get("error"))
		}
		incomplete := kind == "response.incomplete" || response.Get("status").String() == "incomplete"
		for i, item := range response.Get("output").Array() {
			if incomplete && item.Get("type").String() == "function_call" && !s.finished[i] && item.Get("status").String() != "completed" {
				// A response cut at an output limit may contain a syntactically valid
				// prefix of tool arguments. Do not finalize that call for execution.
				return io.ErrUnexpectedEOF
			}
			if !s.finished[i] {
				if err := s.itemDone(r, i, item); err != nil {
					return err
				}
			}
		}
		if len(s.reasoning) > 0 {
			return io.ErrUnexpectedEOF
		}
		if incomplete {
			for _, i := range r.toolIndex {
				if !r.blocks[i].closed {
					return io.ErrUnexpectedEOF
				}
			}
			switch response.Get("incomplete_details.reason").String() {
			case "max_output_tokens":
				r.stopReason = "max_tokens"
			case "content_filter":
				r.stopReason = "refusal"
			case "compaction_required_for_model_handoff":
				return fmt.Errorf("Kiro requires context compaction for model handoff")
			default:
				return fmt.Errorf("unknown reason for incomplete Kiro response")
			}
		}
		r.terminal = true
		return r.finish()
	}
	return nil
}

func (r *kiroStream) readResponsesUsage(usage ojson.Result) {
	if !usage.IsObject() {
		return
	}
	r.readUsage(usage)
	if usage.Get("kiro:credits.value").Type == ojson.Number {
		r.noteMetering()
	}
	if usage.Get("kiro:context_usage_percentage").Type == ojson.Number {
		r.noteContextPercentage()
	}
}

func (s *kiroSSE) toolStart(r *kiroStream, index int, item ojson.Result) error {
	id := item.Get("call_id").String()
	if id == "" {
		id = item.Get("id").String()
	}
	if id == "" || item.Get("name").String() == "" {
		return fmt.Errorf("incomplete Kiro function call")
	}
	if old, ok := s.calls[index]; ok && old != id {
		return fmt.Errorf("conflicting Kiro function call")
	}
	s.calls[index] = id
	return r.toolPart(id, item.Get("name").String(), item.Get("arguments").String(), false)
}

func (s *kiroSSE) toolDone(r *kiroStream, index int, full string) error {
	id, ok := s.calls[index]
	if !ok {
		return fmt.Errorf("missing Kiro function call")
	}
	block := r.blocks[r.toolIndex[id]]
	sent := block.input.String()
	if full != "" && !strings.HasPrefix(full, sent) {
		return fmt.Errorf("inconsistent Kiro function arguments")
	}
	if block.closed {
		if full != "" && full != sent {
			return fmt.Errorf("conflicting completed Kiro arguments")
		}
		return nil
	}
	suffix := ""
	if full != "" {
		suffix = full[len(sent):]
	}
	return r.toolPart(id, "", suffix, true)
}

func (s *kiroSSE) itemDone(r *kiroStream, index int, item ojson.Result) error {
	if s.finished[index] {
		return nil
	}
	switch item.Get("type").String() {
	case "function_call":
		if item.Get("status").String() == "incomplete" {
			return fmt.Errorf("incomplete Kiro function call")
		}
		if old := s.calls[index]; old != "" && item.Get("call_id").Exists() && item.Get("call_id").String() != old {
			return fmt.Errorf("conflicting Kiro function call ID")
		}
		if _, ok := s.calls[index]; !ok {
			if err := s.toolStart(r, index, item); err != nil {
				return err
			}
		}
		if err := s.toolDone(r, index, item.Get("arguments").String()); err != nil {
			return err
		}
	case "reasoning":
		thought := s.reasoning[index]
		delete(s.reasoning, index)
		if raw := item.Get("encrypted_content").String(); raw != "" {
			var b kiroReasoning
			if err := json.Unmarshal([]byte(raw), &b); err != nil || (b.Text == nil) == (b.Redacted == "") {
				return fmt.Errorf("invalid Kiro reasoning envelope")
			}
			if b.Text != nil {
				r.openText("thinking")
				r.text("thinking", b.Text.Text)
				if b.Text.Signature != "" {
					r.signature(b.Text.Signature)
				}
			}
			if b.Redacted != "" {
				if err := r.redacted(b.Redacted); err != nil {
					return err
				}
			}
		} else if thought != nil {
			r.text("thinking", thought.String())
		} else {
			for _, part := range item.Get("summary").Array() {
				if part.Get("type").String() == "summary_text" {
					r.text("thinking", part.Get("text").String())
				}
			}
		}
		r.closeText()
	case "message":
		for j, p := range item.Get("content").Array() {
			if len(p.Get("annotations").Array()) > 0 {
				r.noteCodeReferences()
			}
			if s.textSeen[[2]int{index, j}] {
				continue
			}
			switch p.Get("type").String() {
			case "output_text":
				r.text("text", p.Get("text").String())
			case "refusal":
				r.text("text", p.Get("refusal").String())
			}
		}
		r.closeText()
	}
	s.finished[index] = true
	return nil
}
