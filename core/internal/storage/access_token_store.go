package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

const AccessTokenLimit = 100

type AccessTokenSource string

const (
	AccessTokenSourceSystemDefault AccessTokenSource = "system_default"
	AccessTokenSourceUser          AccessTokenSource = "user"
)

func (source AccessTokenSource) Valid() bool {
	return source == AccessTokenSourceSystemDefault || source == AccessTokenSourceUser
}

// AccessTokenMetadata is safe to return from list operations. It deliberately
// contains neither the bearer value nor its SHA-256 digest. Source tells the
// bootstrap token this installation issued apart from the ones a user created.
type AccessTokenMetadata struct {
	contract.AccessTokenRouting
	RoutingRevision int64                  `json:"routing_revision"`
	ID              contract.AccessTokenID `json:"id"`
	Name            string                 `json:"name"`
	Hint            string                 `json:"hint"`
	Source          AccessTokenSource      `json:"source"`
	CreatedAt       time.Time              `json:"created_at"`
}

func (token AccessTokenMetadata) ETag() string {
	raw, _ := json.Marshal(token)
	return fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
}

type AccessTokenHash [32]byte

// NewAccessToken carries secret material across the narrow Manager-to-Store
// boundary. Store implementations must persist Value separately from metadata
// and never return Hash or Value from list operations.
type NewAccessToken struct {
	contract.AccessTokenRouting
	ID      contract.AccessTokenID
	Name    string
	NameKey string
	Hash    AccessTokenHash
	Hint    string
	Value   string
}

type AccessTokenStore interface {
	EnsureDefaultAccessToken(context.Context, NewAccessToken) (AccessTokenMetadata, bool, error)
	CreateAccessToken(context.Context, NewAccessToken) (AccessTokenMetadata, error)
	ListAccessTokens(context.Context) ([]AccessTokenMetadata, error)
	RevealAccessToken(context.Context, contract.AccessTokenID) (string, error)
	DeleteAccessToken(context.Context, contract.AccessTokenID) error
	FindAccessTokenByHash(context.Context, AccessTokenHash) (AccessTokenMetadata, error)
}

// AccessTokenRoutingStore owns conditional scope changes separately from
// authentication and secret retrieval.
type AccessTokenRoutingStore interface {
	GetAccessToken(context.Context, contract.AccessTokenID) (AccessTokenMetadata, error)
	UpdateAccessTokenRouting(context.Context, contract.AccessTokenID, contract.AccessTokenRouting, string) (AccessTokenMetadata, error)
}
