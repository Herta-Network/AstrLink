package providerapi

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func kiroFrame(kind, body string) []byte { return kiroTypedFrame("event", kind, body) }
func kiroTypedFrame(messageType, kind, body string) []byte {
	var headers bytes.Buffer
	for _, pair := range [][2]string{{":message-type", messageType}, {":event-type", kind}} {
		headers.WriteByte(byte(len(pair[0])))
		headers.WriteString(pair[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(pair[1])))
		headers.WriteString(pair[1])
	}
	frame := make([]byte, 12)
	binary.BigEndian.PutUint32(frame[:4], uint32(16+headers.Len()+len(body)))
	binary.BigEndian.PutUint32(frame[4:8], uint32(headers.Len()))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, headers.Bytes()...)
	frame = append(frame, body...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
}

func TestKiroRequestPreservesConversationToolsAndIdentity(t *testing.T) {
	body := `{"model":"claude-sonnet-4.6","stream":true,"max_tokens":512,"temperature":0,"system":[{"type":"text","text":"User's AstrLink instructions"}],"tools":[{"name":"read_file","description":"AstrLink tool","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}],"messages":[{"role":"user","content":"Read AstrLink"},{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"read_file","input":{"path":"AstrLink/main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":[{"type":"text","text":"file content"}]}]}]}`
	req := httptest.NewRequest("POST", "https://example.test/v1/messages?caller=secret", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer upstream-test-token")
	req.Header.Set("X-AstrLink-Injected", "drop")
	req.Header.Set("User-Agent", "AstrLink")
	req.Header.Set("Originator", "AstrLink")
	exchange, err := KiroRequest(req, "arn:test-profile", "")
	if err != nil {
		t.Fatal(err)
	}
	if !exchange.Streaming || exchange.Model != "claude-sonnet-4.6" {
		t.Fatalf("exchange=%+v", exchange)
	}
	raw, _ := io.ReadAll(req.Body)
	root := gjson.ParseBytes(raw)
	if req.URL.Path != "/generateAssistantResponse" || req.URL.RawQuery != "" {
		t.Fatalf("url=%s", req.URL)
	}
	for name, values := range req.Header {
		if strings.Contains(strings.ToLower(name+strings.Join(values, "")), "astrlink") {
			t.Fatalf("gateway identity escaped in %s", name)
		}
	}
	if req.Header.Get("Authorization") != "Bearer upstream-test-token" || !strings.Contains(req.UserAgent(), "KiroIDE-") {
		t.Fatal("missing provider identity")
	}
	if root.Get("conversationState.history.0.userInputMessage.content").String() != "User's AstrLink instructions" {
		t.Fatal("system prompt changed")
	}
	if root.Get("conversationState.history.3.assistantResponseMessage.toolUses.0.input.path").String() != "AstrLink/main.go" {
		t.Fatal("tool history changed")
	}
	current := root.Get("conversationState.currentMessage.userInputMessage")
	if current.Get("userInputMessageContext.toolResults.0.content.0.text").String() != "file content" {
		t.Fatal("tool result lost")
	}
	if current.Get("userInputMessageContext.tools.0.toolSpecification.inputSchema.json.additionalProperties").Bool() {
		t.Fatal("schema changed")
	}
	if root.Get("inferenceConfig.temperature").Raw != "0" {
		t.Fatal("zero temperature omitted")
	}
	if root.Get("profileArn").String() != "arn:test-profile" {
		t.Fatal("profile missing")
	}
}

func TestKiroRequestRejectsUnsupportedContentInsteadOfDroppingIt(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.test/a.pdf"}}]}]}`,
		`{"model":"m","tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"prefill"}]}`,
	} {
		req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(body))
		if _, err := KiroRequest(req, "", ""); err == nil {
			t.Fatalf("accepted lossy conversion: %s", body)
		}
	}
}

func TestKiroResponseStreamingAndJSON(t *testing.T) {
	frames := bytes.Join([][]byte{
		kiroFrame("reasoningContentEvent", `{"text":"consider"}`),
		kiroFrame("assistantResponseEvent", `{"content":"hello "}`),
		kiroFrame("assistantResponseEvent", `{"content":"hello "}`),
		kiroFrame("toolUseEvent", `{"toolUseId":"a","name":"one","input":"{\"x\":"}`),
		kiroFrame("toolUseEvent", `{"toolUseId":"b","name":"two","input":"{\"y\":2}","stop":true}`),
		kiroFrame("toolUseEvent", `{"toolUseId":"a","input":"1}","stop":true}`),
		kiroFrame("meteringEvent", `{"usage":{"inputTokens":12,"outputTokens":8,"cacheReadInputTokens":4}}`),
	}, nil)
	for _, streaming := range []bool{false, true} {
		res := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(bytes.NewReader(frames))}
		if err := KiroResponse(res, &KiroExchange{Model: "m", Streaming: streaming}); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if streaming {
			text := string(raw)
			if strings.Count(text, `"text":"hello "`) != 2 || !strings.Contains(text, `"stop_reason":"tool_use"`) || !strings.Contains(text, "event: message_stop") {
				t.Fatalf("bad SSE: %s", raw)
			}
			if !strings.Contains(text, `"partial_json":"1}"`) {
				t.Fatalf("missing tool continuation: %s", raw)
			}
		} else {
			root := gjson.ParseBytes(raw)
			if root.Get("content.1.text").String() != "hello hello " || root.Get("content.2.input.x").Int() != 1 || root.Get("content.3.input.y").Int() != 2 || root.Get("usage.output_tokens").Int() != 8 || root.Get("usage.input_tokens").Int() != 8 || root.Get("usage.cache_read_input_tokens").Int() != 4 {
				t.Fatalf("bad message: %s", raw)
			}
		}
	}
}

func TestKiroResponseRejectsCorruptionTruncationAndExceptions(t *testing.T) {
	valid := kiroFrame("assistantResponseEvent", `{"content":"partial"}`)
	corrupt := append([]byte{}, valid...)
	corrupt[len(corrupt)-1] ^= 1
	badPrelude := append([]byte{}, valid...)
	badPrelude[8] ^= 1
	unfinishedTool := append(kiroFrame("toolUseEvent", `{"toolUseId":"a","name":"danger","input":"{\"x\":"}`), kiroFrame("meteringEvent", `{}`)...)
	for name, frames := range map[string][]byte{"empty": nil, "no trailer": valid, "CRC": corrupt, "prelude": badPrelude, "truncated": valid[:len(valid)-2], "tool arguments": unfinishedTool, "exception": kiroTypedFrame("exception", "ThrottlingException", `{"message":"private provider detail"}`)} {
		t.Run(name, func(t *testing.T) {
			for _, streaming := range []bool{false, true} {
				res := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/vnd.amazon.eventstream"}}, Body: io.NopCloser(bytes.NewReader(frames))}
				err := KiroResponse(res, &KiroExchange{Model: "m", Streaming: streaming})
				var raw []byte
				if err == nil {
					raw, err = io.ReadAll(res.Body)
				}
				_ = res.Body.Close()
				if err == nil {
					t.Fatal("invalid stream succeeded")
				}
				if strings.Contains(string(raw), "message_stop") || strings.Contains(err.Error(), "private provider detail") {
					t.Fatalf("false success or leaked error: %s %v", raw, err)
				}
			}
		})
	}
}

func TestKiroToolSchemaRetainsLargeNumbers(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"call","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}]}`
	req := httptest.NewRequest("POST", "https://example.test/v1/messages", strings.NewReader(body))
	if _, err := KiroRequest(req, "", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(req.Body)
	if !json.Valid(raw) || !bytes.Contains(raw, []byte("9007199254740993")) {
		t.Fatal("schema precision lost")
	}
}
