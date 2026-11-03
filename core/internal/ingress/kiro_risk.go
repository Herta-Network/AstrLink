package ingress

import (
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/tidwall/gjson"
)

// Kiro distinguishes exhausted monthly credits from transient capacity and
// throttling. Do not suspend an account merely because an endpoint returns 429.
func classifyKiroRisk(status int, header http.Header, body []byte, now time.Time) subscriptionRiskSignal {
	if status == http.StatusUnauthorized {
		return subscriptionRiskSignal{unauthorized: true}
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return subscriptionRiskSignal{}
	}
	root := gjson.ParseBytes(body)
	message := root.Get("message").String()
	if message == "" {
		message = root.Get("error.message").String()
	}
	reason := root.Get("reason").String()
	if reason == "" {
		reason = root.Get("error.reason").String()
	}
	kind := root.Get("__type").String()
	if kind == "" {
		kind = root.Get("error.type").String()
	}
	if kind == "" {
		kind = root.Get("type").String()
	}
	_, short, found := strings.Cut(kind, "#")
	if found {
		kind = short
	}
	lower := strings.ToLower(message)
	if status == http.StatusForbidden && (strings.EqualFold(reason, "TEMPORARILY_SUSPENDED") || kind == "AccountSuspendedException" || strings.Contains(lower, "temporarily suspended")) {
		return suspendedRisk(contract.RiskCodeAccountDeactivated, message, status)
	}
	if (status == http.StatusTooManyRequests || status == http.StatusPaymentRequired) &&
		(strings.EqualFold(reason, "MONTHLY_REQUEST_COUNT") || strings.Contains(lower, "quota exhausted") || strings.Contains(lower, "usage limit") || strings.Contains(lower, "monthly request")) {
		delay := max(parseRetryAfter(header.Get("Retry-After"), now), subscriptionRiskFallbackPause)
		delay = min(delay, subscriptionRiskMaximumPause)
		return coolingRisk(contract.RiskCodeUsageLimitReached, message, status, now.Add(delay))
	}
	if status == http.StatusTooManyRequests {
		return subscriptionRiskSignal{scope: subscriptionRiskModel, cooldown: min(max(parseRetryAfter(header.Get("Retry-After"), now), time.Minute), subscriptionRiskMaximumPause)}
	}
	return subscriptionRiskSignal{}
}
