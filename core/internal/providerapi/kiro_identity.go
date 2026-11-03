package providerapi

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
)

const DefaultKiroClientVersion = "0.11.107"
const KiroSessionHeader = "X-AstrLink-Kiro-Session-Scope"

// Kiro profile regions may differ from the account's OIDC region. Validate the
// complete ARN before deriving the data-plane hostname from it.
func KiroProfileRegion(arn string) string {
	if len(arn) > 512 {
		return ""
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || (parts[1] != "aws" && parts[1] != "aws-us-gov") || parts[2] != "codewhisperer" || !contract.ValidKiroRegion(parts[3]) || len(parts[4]) != 12 || !strings.HasPrefix(parts[5], "profile/") {
		return ""
	}
	for _, r := range parts[4] {
		if r < '0' || r > '9' {
			return ""
		}
	}
	id := strings.TrimPrefix(parts[5], "profile/")
	if id == "" {
		return ""
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("+=,.@_-", r)) {
			return ""
		}
	}
	return parts[3]
}

// KiroIdentity contains only the paired client fingerprint. Authentication,
// invocation IDs and machine IDs are never persisted by the identity registry.
type KiroIdentity struct{ Version, UserAgent, AmzUserAgent string }

func KiroIdentityAt(version string, streaming bool) KiroIdentity {
	if !contract.ValidClientVersion(version) {
		version = DefaultKiroClientVersion
	}
	sdk, api, mode := "1.0.0", "codewhispererruntime", "m/N,E"
	if streaming {
		sdk, api, mode = "1.0.34", "codewhispererstreaming", "m/E"
	}
	return KiroIdentity{Version: version,
		UserAgent:    "aws-sdk-js/" + sdk + " ua/2.1 lang/js md/nodejs#22.22.0 api/" + api + "#" + sdk + " " + mode + " KiroIDE-" + version,
		AmzUserAgent: "aws-sdk-js/" + sdk + " KiroIDE-" + version,
	}
}

func ApplyKiroIdentity(header http.Header, identity KiroIdentity) {
	header.Set("User-Agent", identity.UserAgent)
	header.Set("X-Amz-User-Agent", identity.AmzUserAgent)
}

// RecognizeKiroIdentity requires a matching AWS SDK / KiroIDE header pair.
// A product word in an arbitrary User-Agent is insufficient. Machine suffixes
// may pass through on this request, but callers learn only Version.
func RecognizeKiroIdentity(header http.Header) (KiroIdentity, bool) {
	ua, amz := header.Get("User-Agent"), header.Get("X-Amz-User-Agent")
	for _, name := range []string{"User-Agent", "X-Amz-User-Agent"} {
		value := header.Get(name)
		if len(header.Values(name)) != 1 || len(value) > 1024 || strings.Contains(strings.ToLower(value), "astrlink") {
			return KiroIdentity{}, false
		}
		for _, r := range value {
			if r < 0x20 || r > 0x7e {
				return KiroIdentity{}, false
			}
		}
	}
	uaFields, amzFields := strings.Fields(ua), strings.Fields(amz)
	if len(uaFields) < 3 || len(amzFields) != 2 || uaFields[0] != amzFields[0] {
		return KiroIdentity{}, false
	}
	sdk, ok := strings.CutPrefix(uaFields[0], "aws-sdk-js/")
	if !ok || !contract.ValidClientVersion(sdk) {
		return KiroIdentity{}, false
	}
	marker := amzFields[1]
	version, ok := strings.CutPrefix(marker, "KiroIDE-")
	if !ok {
		return KiroIdentity{}, false
	}
	if split := strings.LastIndexByte(version, '-'); split >= 0 {
		suffix := version[split+1:]
		machine := len(suffix) == 64
		for _, r := range suffix {
			machine = machine && ((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f'))
		}
		if machine {
			version = version[:split]
		}
	}
	if !contract.ValidClientVersion(version) {
		return KiroIdentity{}, false
	}
	markers, api := 0, false
	for _, field := range uaFields {
		if strings.HasPrefix(field, "KiroIDE-") {
			if field != marker {
				return KiroIdentity{}, false
			}
			markers++
		}
		if field == "api/codewhispererstreaming#"+sdk || field == "api/codewhispererruntime#"+sdk {
			api = true
		}
	}
	if markers != 1 || !api {
		return KiroIdentity{}, false
	}
	return KiroIdentity{Version: version, UserAgent: ua, AmzUserAgent: amz}, true
}
