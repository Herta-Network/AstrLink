package providerapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ErrKiroThinkingUnsupported identifies signed or opaque reasoning the Kiro
// wire cannot carry. The ingress repair policy decides whether to remove it.
var ErrKiroThinkingUnsupported = errors.New("Kiro cannot carry signed or encrypted thinking history")

// KiroStreamError retains structured provider fields for local risk handling,
// while Error never echoes the provider's potentially sensitive message.
type KiroStreamError struct {
	Status int
	Body   []byte
}

func (*KiroStreamError) Error() string { return "Kiro upstream returned an event-stream exception" }

func kiroStreamError(headers map[string]string, event gjson.Result) error {
	kind := headers[":exception-type"]
	if kind == "" {
		kind = headers[":error-code"]
	}
	if kind == "" {
		kind = headers[":event-type"]
	}
	status := http.StatusBadGateway
	switch strings.ToLower(kind) {
	case "throttlingexception", "servicequotaexceededexception":
		status = http.StatusTooManyRequests
	case "accessdeniedexception", "accountsuspendedexception":
		status = http.StatusForbidden
	case "unauthorizedexception", "unrecognizedclientexception", "expiredtokenexception":
		status = http.StatusUnauthorized
	case "validationexception":
		status = http.StatusBadRequest
	}
	message := event.Get("message").String()
	if len(message) > 4096 {
		message = "Kiro rejected the request"
	}
	reason := event.Get("reason").String()
	if len(reason) > 128 {
		reason = ""
	}
	body, _ := json.Marshal(map[string]string{"__type": kind, "message": message, "reason": reason})
	return &KiroStreamError{Status: status, Body: body}
}
