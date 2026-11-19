package contract

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type KiroAuthorization struct {
	Method   string `json:"method"`
	Region   string `json:"region,omitempty"`
	StartURL string `json:"start_url,omitempty"`
}

var kiroRegionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z0-9]+)+-[0-9]+$`)

func ValidKiroRegion(region string) bool {
	return len(region) <= 40 && kiroRegionPattern.MatchString(region)
}

func (options KiroAuthorization) Normalize(flow AuthorizationFlow) (KiroAuthorization, error) {
	if options.Method == "" {
		options.Method = "builder_id"
	}
	if options.Region == "" {
		options.Region = "us-east-1"
	}
	if !ValidKiroRegion(options.Region) {
		return options, fmt.Errorf("invalid Kiro AWS region")
	}
	switch options.Method {
	case "builder_id":
		if flow != AuthorizationFlowDeviceCode || options.StartURL != "" {
			return options, fmt.Errorf("Builder ID requires device_code without an enterprise Start URL")
		}
	case "social":
		if (flow != AuthorizationFlowBrowser && flow != AuthorizationFlowCode) || options.StartURL != "" || options.Region != "us-east-1" {
			return options, fmt.Errorf("Google/GitHub requires browser authorization in us-east-1")
		}
	case "iam_sso":
		if flow != AuthorizationFlowBrowser && flow != AuthorizationFlowCode && flow != AuthorizationFlowDeviceCode {
			return options, fmt.Errorf("unsupported enterprise SSO flow")
		}
		u, err := url.Parse(options.StartURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(options.StartURL) > 2048 || strings.TrimSpace(options.StartURL) != options.StartURL {
			return options, fmt.Errorf("enterprise Start URL must be an HTTPS URL without credentials or query parameters")
		}
	default:
		return options, fmt.Errorf("unknown Kiro sign-in method")
	}
	return options, nil
}

// KiroSocialGrant is the part of a Google or GitHub export used to obtain a fresh grant.
type KiroSocialGrant struct {
	Provider     string `json:"provider"`
	RefreshToken string `json:"refreshToken"`
}

func (grant KiroSocialGrant) Valid() bool {
	return (strings.EqualFold(grant.Provider, "Google") || strings.EqualFold(grant.Provider, "GitHub")) &&
		len(grant.RefreshToken) > 0 && len(grant.RefreshToken) <= 32768 &&
		strings.TrimSpace(grant.RefreshToken) == grant.RefreshToken
}

// KiroProcessingProtocol returns the format selected by a fixed Kiro entry.
// Other entries follow their model pin or configured conversion target.
func (kind ServiceKind) KiroProcessingProtocol(entry ProtocolID) ProtocolID {
	if kind == ServiceKindKiroSubscription {
		switch entry {
		case ProtocolOpenAIResponses, ProtocolOpenAIChat:
			return ProtocolOpenAIResponses
		case ProtocolAnthropicMessages:
			return ProtocolAnthropicMessages
		}
	}
	return ""
}
