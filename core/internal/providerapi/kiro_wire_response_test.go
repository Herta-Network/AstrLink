package providerapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func kiroWireReply(t *testing.T, frames []byte, responses, stream bool) ([]byte, error) {
	t.Helper()
	media := "application/vnd.amazon.eventstream"
	if responses {
		media = "text/event-stream"
	}
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {media}}, Body: io.NopCloser(bytes.NewReader(frames))}
	err := KiroResponse(res, &KiroExchange{Model: "model", Responses: responses, Streaming: stream})
	defer res.Body.Close()
	if err != nil {
		return nil, err
	}
	return io.ReadAll(res.Body)
}

func TestKiroWireTokenUsage(t *testing.T) {
	message := kiroFrame("assistantResponseEvent", `{"content":"hello"}`)
	for _, tc := range []struct {
		name      string
		data      []byte
		responses bool
		want      map[string]int64
	}{
		{"GAR disjoint counts", append(bytes.Clone(message), kiroFrame("metadataEvent", `{"stopReason":"END_TURN","tokenUsage":{"uncachedInputTokens":23,"outputTokens":7,"totalTokens":45,"cacheReadInputTokens":12,"cacheWriteInputTokens":3,"contextUsagePercentage":0.07,"normalizedTokenUsage":4567.2}}`)...), false, map[string]int64{"input_tokens": 23, "output_tokens": 7, "cache_read_input_tokens": 12, "cache_creation_input_tokens": 3}},
		{"GAR reported zero versus missing", append(bytes.Clone(message), kiroFrame("metadataEvent", `{"stopReason":"END_TURN","tokenUsage":{"uncachedInputTokens":0,"cacheReadInputTokens":0,"totalTokens":0}}`)...), false, map[string]int64{"input_tokens": 0, "cache_read_input_tokens": 0}},
		{"GAR metering only", bytes.Join([][]byte{message, kiroFrame("contextUsageEvent", `{"contextUsagePercentage":0.02}`), kiroFrame("meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.125}`)}, nil), false, map[string]int64{}},
		{"Responses cached read and write", []byte("data: " + `{"type":"response.output_text.delta","delta":"hi"}` + "\n\n" + "data: " + `{"type":"response.completed","response":{"usage":{"input_tokens":17,"output_tokens":4,"input_tokens_details":{"cached_tokens":9,"cache_write_tokens":3}}}}` + "\n\n"), true, map[string]int64{"input_tokens": 17, "output_tokens": 4, "input_tokens_details.cached_tokens": 9, "input_tokens_details.cache_write_tokens": 3}},
		{"Responses credits only", []byte("data: " + `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"kiro:credits":{"value":0.125,"unit":"credit","unit_plural":"credits"},"kiro:context_usage_percentage":0.55}}}` + "\n\n"), true, map[string]int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{true, false} {
				out, err := kiroWireReply(t, tc.data, tc.responses, stream)
				if err != nil {
					t.Fatal(err)
				}
				var usage ojson.Result
				if stream {
					for _, line := range strings.Split(string(out), "\n") {
						raw, data := strings.CutPrefix(line, "data: ")
						if !data {
							continue
						}
						switch ojson.Get(raw, "type").String() {
						case "message_start":
							if len(ojson.Get(raw, "message.usage").Map()) != 0 {
								t.Fatal("early count claimed before the upstream supplied usage")
							}
						case "response.completed":
							usage = ojson.Get(raw, "response.usage")
						case "message_delta":
							usage = ojson.Get(raw, "usage")
						}
					}
				} else {
					usage = ojson.GetBytes(out, "usage")
				}
				if !usage.IsObject() || (!tc.responses && len(usage.Map()) != len(tc.want)) {
					t.Fatalf("unexpected count presence: %s", usage.Raw)
				}
				for name, want := range tc.want {
					if v := usage.Get(name); v.Type != ojson.Number || v.Int() != want {
						t.Fatalf("%s: got %s, expected %d", name, v.Raw, want)
					}
				}
			}
		})
	}
}

func TestKiroWireTerminationAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, stop string
		responses            bool
	}{
		{"GAR empty refusal", `{"stopReason":"CONTENT_FILTERED","stopDetails":{"refusal":{"category":"filtered"}}}`, "refusal", false},
		{"GAR pause", `{"stopReason":"PAUSE_TURN"}`, "pause_turn", false},
		{"GAR context window", `{"stopReason":"MODEL_CONTEXT_WINDOW_EXCEEDED"}`, "model_context_window_exceeded", false},
		{"unknown GAR stop", `{"stopReason":"UNRECOGNIZED"}`, "", false},
		{"Responses incomplete in status", `{"type":"response.completed","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_tokens", true},
		{"Responses handoff", `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"compaction_required_for_model_handoff"}}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := kiroFrame("metadataEvent", tc.terminal)
			if tc.responses {
				data = []byte("data: " + tc.terminal + "\n\n")
			}
			for _, stream := range []bool{false, true} {
				out, err := kiroWireReply(t, data, tc.responses, stream)
				if tc.responses {
					if err != nil || !bytes.Contains(out, []byte("incomplete_details")) || bytes.Contains(out, []byte("message_stop")) {
						t.Fatalf("incomplete Responses terminal changed: %s %v", out, err)
					}
					continue
				}
				if tc.stop == "" {
					if err == nil || bytes.Contains(out, []byte("message_stop")) {
						t.Fatalf("expected explicit failure: %s %v", out, err)
					}
					continue
				}
				if err != nil || !bytes.Contains(out, []byte(`"stop_reason":"`+tc.stop+`"`)) {
					t.Fatalf("terminal=%s err=%v", out, err)
				}
				if stream && !bytes.Contains(out, []byte("message_start")) {
					t.Fatal("empty response emitted no message_start")
				}
			}
		})
	}
	for _, tc := range []struct {
		code   string
		status int
		reason string
	}{
		{"context_length_exceeded", 413, "CONTENT_LENGTH_EXCEEDS_THRESHOLD"}, {"model_not_found", 404, "INVALID_MODEL_ID"},
		{"kiro:model_requires_user_opt_in", 403, "MODEL_REQUIRES_USER_OPT_IN"}, {"rate_limit_exceeded", 429, "USER_REQUEST_RATE_EXCEEDED"},
		{"server_is_overloaded", 503, "MODEL_TEMPORARILY_UNAVAILABLE"}, {"credit_balance_exhausted", 402, "CREDIT_BALANCE_EXHAUSTED"},
	} {
		out, err := kiroWireReply(t, []byte("data: "+`{"type":"response.failed","response":{"error":{"code":"`+tc.code+`","message":"sensitive detail","private_field":"never retained"}}}`+"\n\n"), true, true)
		var streamErr *KiroStreamError
		if !errors.As(err, &streamErr) || streamErr.Status != tc.status || ojson.GetBytes(streamErr.Body, "reason").String() != tc.reason || ojson.GetBytes(streamErr.Body, "private_field").Exists() || strings.Contains(err.Error(), "sensitive detail") || bytes.Contains(out, []byte("message_stop")) {
			t.Errorf("unexpected classification for %s: %v", tc.code, err)
		}
	}
}

func TestKiroRetryAfterMilliseconds(t *testing.T) {
	for _, tc := range []struct{ ms, existing, want string }{{"1501", "", "2"}, {"0", "", "0"}, {"-8", "", ""}, {"invalid", "", ""}, {"1501", "7", "2"}, {"1000.25", "", "2"}, {"invalid", "7", "7"}} {
		r := &http.Response{StatusCode: 429, Header: make(http.Header)}
		r.Header.Set("X-Amzn-Kiro-Ratelimit-Retry-After", tc.ms)
		if tc.existing != "" {
			r.Header.Set("Retry-After", tc.existing)
		}
		if err := KiroResponse(r, &KiroExchange{Responses: true}); err != nil || r.Header.Get("Retry-After") != tc.want {
			t.Errorf("ms %s existing %s: %s (%v)", tc.ms, tc.existing, r.Header.Get("Retry-After"), err)
		}
	}
}

func TestKiroFailedTerminalUsageIsPartial(t *testing.T) {
	res := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: " + `{"type":"response.failed","response":{"error":{"type":"server_error"},"usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n")),
	}
	exchange := &KiroExchange{Responses: true, Streaming: true}
	if err := KiroResponse(res, exchange); err != nil {
		t.Fatal(err)
	}
	_, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	usage := exchange.ObservedUsage()
	if err == nil || usage == nil || usage.InputTokens != 7 || usage.OutputTokens != 0 || !usage.BillingIncomplete {
		t.Fatalf("failed terminal claims success or lost counted work: %+v, %v", usage, err)
	}
}

func TestKiroIncompleteResponseDoesNotFinishUnconfirmedTool(t *testing.T) {
	const event = `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call","name":"write","arguments":"{}"}]}}`
	for _, stream := range []bool{false, true} {
		body, err := kiroWireReply(t, []byte("data: "+event+"\n\n"), true, stream)
		if err != nil || !bytes.Contains(body, []byte("max_output_tokens")) || bytes.Contains(body, []byte(`"status":"completed"`)) || bytes.Contains(body, []byte("content_block_stop")) || bytes.Contains(body, []byte("message_stop")) {
			t.Fatalf("incomplete call marked final: %s %v", body, err)
		}
	}
}

func TestKiroNativeResponsesCollectsTextWithoutTerminalOutput(t *testing.T) {
	data := "data: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg-1","delta":"hello"}` + "\n" +
		"data: " + `{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"hello!","annotations":[{"type":"kiro:code_reference"}]}` + "\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp-final","status":"completed","output":[]}}` + "\n"
	out, err := kiroWireReply(t, []byte(data), true, false)
	if err != nil {
		t.Fatal(err)
	}
	r := ojson.ParseBytes(out)
	if r.Get("id").String() != "resp-final" || r.Get("output.0.id").String() != "msg-1" || r.Get("output.0.content.0.text").String() != "hello!" || r.Get("output.0.content.0.annotations.0.type").String() != "kiro:code_reference" {
		t.Fatalf("lost native fields: %s", out)
	}
}

func TestKiroNonStreamingMergesPartialTerminalOutput(t *testing.T) {
	const call = `{"type":"function_call","id":"fc-1","call_id":"call-1","name":"read","arguments":"{}","status":"completed"}`
	const text = `{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"ready"}]}`
	for _, terminal := range []string{text, call, call + "," + text,
		`{"type":"function_call","call_id":"call-1","arguments":""},{"type":"message","id":"msg-1","content":[]}`,
	} {
		data := "data: " + `{"type":"response.output_item.done","output_index":0,"item":` + call + "}\n\n" +
			"data: " + `{"type":"response.output_item.done","output_index":1,"item":` + text + "}\n\n" +
			"data: " + `{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[` + terminal + "]}}\n\n"
		body, err := kiroWireReply(t, []byte(data), true, false)
		if err != nil {
			t.Fatal(err)
		}
		items := ojson.GetBytes(body, "output").Array()
		if len(items) != 2 || items[0].Get("call_id").Str != "call-1" || items[0].Get("arguments").Str != "{}" || items[1].Get("content.0.text").Str != "ready" {
			t.Fatalf("partial terminal lost or duplicated output: %s", body)
		}
	}
}

func TestKiroTerminalCallCanCompleteAnArgumentsOnlyStream(t *testing.T) {
	events := []string{
		`{"type":"response.function_call_arguments.delta","item_id":"fc1","output_index":0,"delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc1"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"id":"fc1","type":"function_call","name":"read","call_id":"call1"}]}}`,
	}
	data := "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
	body, err := kiroWireReply(t, []byte(data), true, false)
	if err != nil || ojson.GetBytes(body, "output.0.arguments").Str != "{}" || ojson.GetBytes(body, "output.0.call_id").Str != "call1" {
		t.Fatalf("incomplete aggregation: %s %v", body, err)
	}
}
