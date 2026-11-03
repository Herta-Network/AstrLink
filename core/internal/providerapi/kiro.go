package providerapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// Protocol reference: https://github.com/tanu360/kiro-reverse-api/tree/a705464b9132c36f578cea5bf563d42e4d978b1f.
// This adapter translates Messages only; cross-protocol conversion is owned by
// RelayKit. Unlike the reference proxy, it never truncates caller history or
// rewrites tool schemas to make a request fit.
const (
	KiroProfileHeader = "X-AstrLink-Kiro-Profile"
	kiroMaxBody       = 32 << 20
)

// ApplyKiroHeaders is shared by inference, authorization and catalog/quota
// requests. The transport creates a fresh header map so caller identities do
// not mix with the provider identity.
func ApplyKiroHeaders(header http.Header, token string, identity KiroIdentity) {
	ApplyKiroIdentity(header, identity)
	header.Set("X-Amzn-Codewhisperer-Optout", "true")
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
}

type KiroExchange struct {
	Model     string
	Streaming bool
}

type kiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}
type kiroToolResult struct {
	ID      string              `json:"toolUseId"`
	Content []map[string]string `json:"content"`
	Status  string              `json:"status"`
}
type kiroTool struct {
	Specification struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema struct {
			JSON json.RawMessage `json:"json"`
		} `json:"inputSchema"`
	} `json:"toolSpecification"`
}
type kiroContext struct {
	Tools   []kiroTool       `json:"tools,omitempty"`
	Results []kiroToolResult `json:"toolResults,omitempty"`
}
type kiroUser struct {
	Content   string         `json:"content"`
	Model     string         `json:"modelId"`
	Origin    string         `json:"origin"`
	Documents []kiroDocument `json:"documents,omitempty"`
	Images    []kiroImage    `json:"images,omitempty"`
	Context   *kiroContext   `json:"userInputMessageContext,omitempty"`
}
type kiroToolUse struct {
	ID    string          `json:"toolUseId"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
type kiroAssistant struct {
	Content string        `json:"content"`
	Tools   []kiroToolUse `json:"toolUses,omitempty"`
}
type kiroTurn struct {
	User      *kiroUser      `json:"userInputMessage,omitempty"`
	Assistant *kiroAssistant `json:"assistantResponseMessage,omitempty"`
}

func KiroRequest(req *http.Request, profile, sessionScope string) (*KiroExchange, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/messages") || req.Body == nil {
		return nil, fmt.Errorf("Kiro requires an Anthropic Messages request")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, kiroMaxBody+1))
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > kiroMaxBody {
		return nil, fmt.Errorf("Kiro request exceeds size limit")
	}
	root := gjson.ParseBytes(raw)
	if !gjson.ValidBytes(raw) || !root.IsObject() || root.Get("model").String() == "" || !root.Get("messages").IsArray() {
		return nil, fmt.Errorf("invalid Kiro Messages request")
	}
	model := root.Get("model").String()
	turns := []kiroTurn{}
	for _, message := range root.Get("messages").Array() {
		turn, err := kiroMessage(message, model)
		if err != nil {
			return nil, err
		}
		if len(turns) > 0 && turn.User != nil && turns[len(turns)-1].User != nil {
			previous := turns[len(turns)-1].User
			previous.Content = kiroJoin(previous.Content, turn.User.Content)
			previous.Images = append(previous.Images, turn.User.Images...)
			previous.Documents = append(previous.Documents, turn.User.Documents...)
			previous.Context.Results = append(previous.Context.Results, turn.User.Context.Results...)
		} else if len(turns) > 0 && turn.Assistant != nil && turns[len(turns)-1].Assistant != nil {
			previous := turns[len(turns)-1].Assistant
			previous.Content = kiroJoin(previous.Content, turn.Assistant.Content)
			previous.Tools = append(previous.Tools, turn.Assistant.Tools...)
		} else {
			turns = append(turns, turn)
		}
	}
	if len(turns) == 0 || turns[0].User == nil || turns[len(turns)-1].User == nil {
		return nil, fmt.Errorf("Kiro requires history starting and ending with a user message; assistant prefill is unsupported")
	}
	first := turns[0].User
	anchor, _ := json.Marshal(struct {
		Text      string
		Images    []kiroImage
		Documents []kiroDocument
	}{first.Content, first.Images, first.Documents})
	current := turns[len(turns)-1].User
	history := turns[:len(turns)-1]
	system, err := kiroText(root.Get("system"))
	if err != nil {
		return nil, err
	}
	if thinking := root.Get("thinking.type").String(); thinking == "enabled" || thinking == "adaptive" {
		budget := root.Get("thinking.budget_tokens").Int()
		if budget <= 0 {
			budget = 16000
		}
		system = kiroJoin(fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", budget), system)
	}
	if system != "" {
		history = append([]kiroTurn{
			{User: &kiroUser{Content: system, Model: model, Origin: "AI_EDITOR"}},
			{Assistant: &kiroAssistant{Content: "I will follow these instructions."}},
		}, history...)
	}
	choice, selected := root.Get("tool_choice.type").String(), root.Get("tool_choice.name").String()
	if rawChoice := root.Get("tool_choice"); rawChoice.Type == gjson.String {
		choice = rawChoice.String()
	}
	if choice == "" {
		choice = "auto"
	}
	if choice != "auto" && choice != "none" && choice != "any" && choice != "tool" {
		return nil, fmt.Errorf("unsupported Kiro tool_choice")
	}
	if choice == "any" && len(root.Get("tools").Array()) == 0 {
		return nil, fmt.Errorf("tool_choice any requires tools")
	}
	if choice == "tool" && selected == "" {
		return nil, fmt.Errorf("tool_choice tool requires a name")
	}
	for _, tool := range root.Get("tools").Array() {
		if choice == "none" {
			break
		}
		name, schema := tool.Get("name").String(), tool.Get("input_schema")
		if choice == "tool" && name != selected {
			continue
		}
		if name == "" || !schema.IsObject() || (tool.Get("type").String() != "" && tool.Get("type").String() != "custom") {
			return nil, fmt.Errorf("Kiro requires named custom tools with object input schemas")
		}
		var converted kiroTool
		converted.Specification.Name = name
		converted.Specification.Description = tool.Get("description").String()
		converted.Specification.InputSchema.JSON = json.RawMessage(schema.Raw)
		current.Context.Tools = append(current.Context.Tools, converted)
	}
	if choice == "tool" && len(current.Context.Tools) == 0 {
		return nil, fmt.Errorf("tool_choice names an undeclared tool")
	}
	if choice == "any" {
		current.Content = kiroJoin(current.Content, "The client requires a tool call on this turn. Call at least one available tool.")
	}
	if choice == "tool" {
		current.Content = kiroJoin(current.Content, fmt.Sprintf("The client requires the tool %q on this turn. Call that tool.", selected))
	}
	if current.Content == "" {
		current.Content = "Continue using the supplied conversation and tool results."
	}
	// A caller-provided UUID is reusable without isolation; otherwise the
	// mapping includes its identity and the stable per-account scope.
	conversationID := root.Get("metadata.session_id").String()
	if conversationID == "" || sessionScope != "" || !kiroUUID(conversationID) {
		seed := conversationID
		if seed == "" {
			seed = model + "\x00" + system + "\x00" + string(anchor)
		}
		mac := hmac.New(sha256.New, []byte(sessionScope))
		_, _ = mac.Write([]byte(seed))
		digest := mac.Sum(nil)[:16]
		digest[6] = (digest[6] & 0x0f) | 0x50
		digest[8] = (digest[8] & 0x3f) | 0x80
		id := hex.EncodeToString(digest)
		conversationID = id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
	}
	for _, turn := range append(history, kiroTurn{User: current}) {
		if turn.User != nil && turn.User.Context != nil && len(turn.User.Context.Tools) == 0 && len(turn.User.Context.Results) == 0 {
			turn.User.Context = nil
		}
		if turn.User != nil {
			uniqueKiroDocumentNames(turn.User.Documents)
		}
	}
	payload := map[string]any{
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL", "agentTaskType": "vibe", "agentContinuationId": randomUUID(),
			"conversationId": conversationID, "currentMessage": kiroTurn{User: current}, "history": history,
		},
	}
	if profile != "" {
		payload["profileArn"] = profile
	}
	inference := map[string]any{}
	for from, to := range map[string]string{"max_tokens": "maxTokens", "temperature": "temperature", "top_p": "topP"} {
		if value := root.Get(from); value.Exists() {
			inference[to] = json.RawMessage(value.Raw)
		}
	}
	if len(inference) > 0 {
		payload["inferenceConfig"] = inference
	}
	if root.Get("stop_sequences").Exists() && len(root.Get("stop_sequences").Array()) > 0 {
		return nil, fmt.Errorf("Kiro does not support stop_sequences")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	identity, recognized := RecognizeKiroIdentity(req.Header)
	header := make(http.Header)
	if !recognized || !strings.Contains(identity.UserAgent, "api/codewhispererstreaming#") {
		identity = KiroIdentityAt(identity.Version, true)
	}
	ApplyKiroHeaders(header, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), identity)
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/vnd.amazon.eventstream")
	header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	header.Set("X-Amzn-Kiro-Agent-Mode", "vibe")
	header.Set("Amz-Sdk-Invocation-Id", randomUUID())
	header.Set("Amz-Sdk-Request", "attempt=1; max=1")
	req.Header = header
	if req.URL.Hostname() == "codewhisperer.us-east-1.amazonaws.com" {
		if region := KiroProfileRegion(profile); region != "" && region != "us-east-1" {
			req.URL.Host = "q." + region + ".amazonaws.com"
			req.Host = ""
		}
	}
	req.URL.Path, req.URL.RawPath, req.URL.RawQuery = "/generateAssistantResponse", "", ""
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.ContentLength = int64(len(body))
	return &KiroExchange{Model: model, Streaming: root.Get("stream").Bool()}, nil
}

func kiroJoin(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n\n" + b
}

func kiroText(value gjson.Result) (string, error) {
	if !value.Exists() || value.Type == gjson.Null {
		return "", nil
	}
	if value.Type == gjson.String {
		return value.String(), nil
	}
	if !value.IsArray() {
		return "", fmt.Errorf("unsupported Kiro text content")
	}
	var parts []string
	for _, block := range value.Array() {
		if block.Get("type").String() != "text" {
			return "", fmt.Errorf("unsupported Kiro text block")
		}
		parts = append(parts, block.Get("text").String())
	}
	return strings.Join(parts, "\n"), nil
}

func kiroMessage(message gjson.Result, model string) (kiroTurn, error) {
	role, content := message.Get("role").String(), message.Get("content")
	if role != "user" && role != "assistant" {
		return kiroTurn{}, fmt.Errorf("unsupported Kiro message role")
	}
	user := &kiroUser{Model: model, Origin: "AI_EDITOR", Context: &kiroContext{}}
	assistant := &kiroAssistant{}
	text := ""
	if content.Type == gjson.String {
		text = content.String()
	} else if content.IsArray() {
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "text":
				text = kiroJoin(text, block.Get("text").String())
			case "redacted_thinking":
				return kiroTurn{}, ErrKiroThinkingUnsupported
			case "thinking":
				if block.Get("signature").String() != "" {
					return kiroTurn{}, ErrKiroThinkingUnsupported
				}
				if role != "assistant" {
					return kiroTurn{}, fmt.Errorf("thinking must belong to an assistant")
				}
				text = kiroJoin(text, "<thinking>"+block.Get("thinking").String()+"</thinking>")
			case "document", "file", "input_file":
				if role != "user" {
					return kiroTurn{}, fmt.Errorf("Kiro documents must belong to a user message")
				}
				doc, err := kiroDocumentBlock(block)
				if err != nil {
					return kiroTurn{}, err
				}
				user.Documents = append(user.Documents, doc)
			case "image":
				if role != "user" || block.Get("source.type").String() != "base64" {
					return kiroTurn{}, fmt.Errorf("Kiro supports base64 user images only")
				}
				format := strings.TrimPrefix(block.Get("source.media_type").String(), "image/")
				if format != "png" && format != "jpeg" && format != "gif" && format != "webp" {
					return kiroTurn{}, fmt.Errorf("unsupported Kiro image format")
				}
				data := block.Get("source.data").String()
				if _, err := base64.StdEncoding.DecodeString(data); err != nil || data == "" {
					return kiroTurn{}, fmt.Errorf("invalid Kiro image data")
				}
				image := kiroImage{Format: format}
				image.Source.Bytes = data
				user.Images = append(user.Images, image)
			case "tool_use":
				if role != "assistant" || block.Get("id").String() == "" || block.Get("name").String() == "" || !block.Get("input").IsObject() {
					return kiroTurn{}, fmt.Errorf("invalid Kiro tool use")
				}
				assistant.Tools = append(assistant.Tools, kiroToolUse{ID: block.Get("id").String(), Name: block.Get("name").String(), Input: json.RawMessage(block.Get("input").Raw)})
			case "tool_result":
				if role != "user" || block.Get("tool_use_id").String() == "" {
					return kiroTurn{}, fmt.Errorf("invalid Kiro tool result")
				}
				resultText, err := kiroToolResultContent(block.Get("content"), user)
				if err != nil {
					return kiroTurn{}, err
				}
				status := "success"
				if block.Get("is_error").Bool() {
					status = "error"
				}
				user.Context.Results = append(user.Context.Results, kiroToolResult{ID: block.Get("tool_use_id").String(), Content: []map[string]string{{"text": resultText}}, Status: status})
			default:
				return kiroTurn{}, fmt.Errorf("unsupported Kiro content block type %q", block.Get("type").String())
			}
		}
	} else {
		return kiroTurn{}, fmt.Errorf("invalid Kiro message content")
	}
	if role == "user" {
		if text == "" && len(user.Documents) > 0 {
			text = "Please analyze the attached document."
		}
		user.Content = text
		return kiroTurn{User: user}, nil
	}
	assistant.Content = text
	return kiroTurn{Assistant: assistant}, nil
}

func kiroUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
