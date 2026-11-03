package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const accessTokenMetadataColumns = `id, name, name_key, token_hint, source, created_at, routing_scope, COALESCE(pool_id, ''), routing_revision`

func (store *Store) GetAccessToken(ctx context.Context, id contract.AccessTokenID) (storage.AccessTokenMetadata, error) {
	if err := id.Validate(); err != nil {
		return storage.AccessTokenMetadata{}, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	return scanAccessTokenMetadata(store.db.QueryRowContext(ctx, `SELECT `+accessTokenMetadataColumns+` FROM local_access_tokens WHERE id = ?`, id))
}

func validateTokenPool(ctx context.Context, tx *sql.Tx, routing contract.AccessTokenRouting) error {
	if err := routing.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if routing.RoutingScope != contract.RoutingScopePool {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_pools WHERE id = ?)`, routing.PoolID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: account pool does not exist", storage.ErrInvalidArgument)
	}
	return nil
}

func (store *Store) UpdateAccessTokenRouting(ctx context.Context, id contract.AccessTokenID, routing contract.AccessTokenRouting, expected string) (record storage.AccessTokenMetadata, err error) {
	if err := id.Validate(); err != nil {
		return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if expected == "" {
		return record, fmt.Errorf("%w: expected ETag is required", storage.ErrInvalidArgument)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer rollbackOnError(tx, &err)
	if err = validateTokenPool(ctx, tx, routing); err != nil {
		return record, err
	}
	record, err = scanAccessTokenMetadata(tx.QueryRowContext(ctx, `SELECT `+accessTokenMetadataColumns+` FROM local_access_tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, storage.ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if record.ETag() != expected {
		return record, storage.ErrPrecondition
	}
	if record.AccessTokenRouting != routing {
		_, err = tx.ExecContext(ctx, `UPDATE local_access_tokens SET routing_scope = ?, pool_id = ?, routing_revision = routing_revision + 1 WHERE id = ?`, routing.RoutingScope, sql.NullString{String: string(routing.PoolID), Valid: routing.PoolID != ""}, id)
		if err != nil {
			return record, err
		}
		record.AccessTokenRouting = routing
		record.RoutingRevision++
	}
	err = tx.Commit()
	if err == nil {
		store.notifyAccountPools()
	}
	return record, err
}
