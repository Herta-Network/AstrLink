package accountauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

const DefaultKiroClientVersion = providerapi.DefaultKiroClientVersion

func kiroIdentityAt(version string) ClientIdentity {
	identity := providerapi.KiroIdentityAt(version, true)
	return ClientIdentity{Version: identity.Version, UserAgent: identity.UserAgent, Headers: map[string]string{"X-Amz-User-Agent": identity.AmzUserAgent}}
}

func (registry *IdentityRegistry) KiroIdentity(settings contract.RoutingSettings) ClientIdentity {
	version := DefaultKiroClientVersion
	if settings.KiroIdentityAutoLearn {
		if learned, ok := registry.learnedIdentity(contract.SubscriptionProviderKiro); ok {
			if order, valid := contract.CompareClientVersions(learned.Version, version); valid && order > 0 {
				version = learned.Version
			}
		}
	}
	if order, valid := contract.CompareClientVersions(settings.KiroIdentityVersion, version); valid && order > 0 {
		version = settings.KiroIdentityVersion
	}
	return kiroIdentityAt(version)
}

func (registry *IdentityRegistry) KiroIdentityFor(ctx context.Context) ClientIdentity {
	return registry.KiroIdentity(registry.routingSettings(ctx))
}

// LearnKiro stores only a validated version. SDK/platform fingerprints,
// machine identifiers, credentials and invocation headers stay on the request.
func (registry *IdentityRegistry) LearnKiro(ctx context.Context, header http.Header) (bool, error) {
	if registry == nil {
		return false, nil
	}
	identity, ok := providerapi.RecognizeKiroIdentity(header)
	if !ok {
		return false, nil
	}
	return registry.learn(ctx, contract.SubscriptionProviderKiro, kiroIdentityAt(identity.Version))
}

func validLearnedKiro(identity ClientIdentity) bool {
	want := kiroIdentityAt(identity.Version)
	if identity.UserAgent != want.UserAgent || len(identity.Headers) != 1 || identity.Headers["X-Amz-User-Agent"] != want.Headers["X-Amz-User-Agent"] {
		return false
	}
	order, valid := contract.CompareClientVersions(identity.Version, DefaultKiroClientVersion)
	major, _ := contract.ClientVersionMajor(identity.Version)
	baseline, _ := contract.ClientVersionMajor(DefaultKiroClientVersion)
	return valid && order >= 0 && major <= baseline+maxLearnedMajorSkew
}

func ApplyKiroForwardHeaders(header http.Header, tokens AccountTokens, clientHeaders http.Header, identity ClientIdentity, enforce bool) {
	ApplyKiroAPIHeaders(header, tokens, identity)
	selected := providerapi.KiroIdentityAt(identity.Version, true)
	if !enforce {
		if client, ok := providerapi.RecognizeKiroIdentity(clientHeaders); ok {
			selected = client
			if !strings.Contains(client.UserAgent, "api/codewhispererstreaming#") {
				selected = providerapi.KiroIdentityAt(client.Version, true)
			}
		}
	}
	providerapi.ApplyKiroIdentity(header, selected)
}
