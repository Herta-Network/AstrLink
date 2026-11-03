package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type poolQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Read metadata and membership in one SQLite snapshot. Inference does not
// need a write transaction merely to resolve a pool or its effective proxy.
func readAccountPools(ctx context.Context, reader poolQueryer, where string, args ...any) ([]storage.AccountPoolRecord, error) {
	rows, err := reader.QueryContext(ctx, `SELECT p.id, p.document_json, m.service_id, m.enabled, m.max_concurrent_requests
FROM account_pools p LEFT JOIN account_pool_members m ON m.pool_id = p.id `+where+`
ORDER BY p.created_at, p.id, m.service_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []storage.AccountPoolRecord{}
	for rows.Next() {
		var id, document string
		var service sql.NullString
		var enabled sql.NullBool
		var limit sql.NullInt64
		if err := rows.Scan(&id, &document, &service, &enabled, &limit); err != nil {
			return nil, err
		}
		if len(result) == 0 || string(result[len(result)-1].Pool.ID) != id {
			var pool contract.AccountPool
			if err := json.Unmarshal([]byte(document), &pool); err != nil {
				return nil, fmt.Errorf("%w: decode pool: %v", storage.ErrInvalidRecord, err)
			}
			if err := pool.Validate(); err != nil || string(pool.ID) != id {
				return nil, fmt.Errorf("%w: invalid pool document", storage.ErrInvalidRecord)
			}
			result = append(result, storage.AccountPoolRecord{Pool: pool, Members: []contract.AccountPoolMember{}})
		}
		record := &result[len(result)-1]
		if service.Valid {
			member := contract.AccountPoolMember{PoolID: record.Pool.ID, ServiceID: contract.ServiceID(service.String), Enabled: enabled.Bool, MaxConcurrentRequests: int(limit.Int64)}
			if err := member.Validate(); err != nil || !enabled.Valid || !limit.Valid {
				return nil, fmt.Errorf("%w: invalid pool membership", storage.ErrInvalidRecord)
			}
			record.Members = append(record.Members, member)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range result {
		raw, err := json.Marshal(struct {
			Pool    contract.AccountPool
			Members []contract.AccountPoolMember
		}{result[index].Pool, result[index].Members})
		if err != nil {
			return nil, err
		}
		result[index].ETag = entityTag(raw)
	}
	return result, nil
}

func readAccountPool(ctx context.Context, reader poolQueryer, id contract.AccountPoolID) (storage.AccountPoolRecord, error) {
	if err := id.Validate(); err != nil {
		return storage.AccountPoolRecord{}, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	records, err := readAccountPools(ctx, reader, `WHERE p.id = ?`, id)
	if err != nil {
		return storage.AccountPoolRecord{}, err
	}
	if len(records) == 0 {
		return storage.AccountPoolRecord{}, fmt.Errorf("%w: account pool %q", storage.ErrNotFound, id)
	}
	return records[0], nil
}

func (store *Store) GetAccountPool(ctx context.Context, id contract.AccountPoolID) (storage.AccountPoolRecord, error) {
	return readAccountPool(ctx, store.db, id)
}

func (store *Store) ListAccountPools(ctx context.Context) ([]storage.AccountPoolRecord, error) {
	return readAccountPools(ctx, store.db, "")
}

func (store *Store) AccountPoolForService(ctx context.Context, id contract.ServiceID) (storage.AccountPoolRecord, bool, error) {
	if err := id.Validate(); err != nil {
		return storage.AccountPoolRecord{}, false, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	records, err := readAccountPools(ctx, store.db, `WHERE p.id = (SELECT pool_id FROM account_pool_members WHERE service_id = ?)`, id)
	if err != nil {
		return storage.AccountPoolRecord{}, false, err
	}
	if len(records) == 0 {
		return storage.AccountPoolRecord{}, false, nil
	}
	return records[0], true, nil
}

// AccountPoolMembership is the small lookup used by proxy resolution and
// quota observation; it does not read every sibling account on each operation.
func (store *Store) AccountPoolMembership(ctx context.Context, id contract.ServiceID) (contract.AccountPool, contract.AccountPoolMember, bool, error) {
	if err := id.Validate(); err != nil {
		return contract.AccountPool{}, contract.AccountPoolMember{}, false, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	records, err := readAccountPools(ctx, store.db, `WHERE m.service_id = ?`, id)
	if err != nil {
		return contract.AccountPool{}, contract.AccountPoolMember{}, false, err
	}
	if len(records) == 0 {
		return contract.AccountPool{}, contract.AccountPoolMember{}, false, nil
	}
	return records[0].Pool, records[0].Members[0], true, nil
}

func (store *Store) CreateAccountPool(ctx context.Context, pool contract.AccountPool, mutation storage.PoolProxyMutation) (storage.AccountPoolRecord, error) {
	return store.writeAccountPool(ctx, pool, mutation, "", true)
}

func (store *Store) UpdateAccountPool(ctx context.Context, pool contract.AccountPool, mutation storage.PoolProxyMutation, expected string) (storage.AccountPoolRecord, error) {
	return store.writeAccountPool(ctx, pool, mutation, expected, false)
}

func (store *Store) writeAccountPool(ctx context.Context, pool contract.AccountPool, mutation storage.PoolProxyMutation, expected string, create bool) (record storage.AccountPoolRecord, err error) {
	if err := pool.ID.Validate(); err != nil {
		return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	if !create && expected == "" {
		return record, fmt.Errorf("%w: expected ETag is required", storage.ErrInvalidArgument)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer rollbackOnError(tx, &err)
	current, lookupErr := readAccountPool(ctx, tx, pool.ID)
	if create {
		if lookupErr == nil {
			return record, fmt.Errorf("%w: pool already exists", storage.ErrConflict)
		}
		if !errors.Is(lookupErr, storage.ErrNotFound) {
			return record, lookupErr
		}
		pool.CreatedAt = store.now().UTC()
	} else {
		if lookupErr != nil {
			return record, lookupErr
		}
		if current.ETag != expected {
			return record, storage.ErrPrecondition
		}
		if pool.Provider != current.Pool.Provider {
			return record, fmt.Errorf("%w: pool provider cannot change", storage.ErrInvalidArgument)
		}
		pool.CreatedAt = current.Pool.CreatedAt
	}
	change := storage.AccountPoolMutation{PoolID: pool.ID}
	for _, member := range current.Members {
		change.Accounts = append(change.Accounts, member.ServiceID)
	}
	release, err := store.beginPoolMutation(ctx, change)
	if err != nil {
		return record, err
	}
	defer func() {
		_ = tx.Rollback()
		release()
	}()
	pool.UpdatedAt = store.now().UTC()
	if pool.Proxy != nil {
		proxy := *pool.Proxy
		pool.Proxy = &proxy
		proxy.CredentialRef = ""
		if proxy.Mode == "custom" && !mutation.Present && current.Pool.Proxy != nil {
			proxy.CredentialRef = current.Pool.Proxy.CredentialRef
		}
	}
	if mutation.Credential != nil {
		if !mutation.Present || pool.Proxy == nil || pool.Proxy.Mode != "custom" {
			return record, fmt.Errorf("%w: proxy credential requires custom mode", storage.ErrInvalidArgument)
		}
		if err := mutation.Credential.Validate(); err != nil {
			return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
		}
		pool.Proxy.CredentialRef = "local://pool-proxy/" + string(pool.ID)
	}
	if err := pool.Validate(); err != nil {
		return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	raw, err := json.Marshal(pool)
	if err != nil {
		return record, err
	}
	if create {
		_, err = tx.ExecContext(ctx, `INSERT INTO account_pools (id, document_json, created_at, updated_at) VALUES (?, ?, ?, ?)`, pool.ID, string(raw), pool.CreatedAt.Format(time.RFC3339Nano), pool.UpdatedAt.Format(time.RFC3339Nano))
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE account_pools SET document_json = ?, updated_at = ? WHERE id = ?`, string(raw), pool.UpdatedAt.Format(time.RFC3339Nano), pool.ID)
	}
	if err != nil {
		return record, err
	}
	if pool.Proxy == nil || pool.Proxy.CredentialRef == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM account_pool_proxy_credentials WHERE pool_id = ?`, pool.ID)
	} else if mutation.Present && mutation.Credential != nil {
		var value, sealed []byte
		value, err = json.Marshal(mutation.Credential)
		if err == nil {
			sealed, err = store.keys.sealColumn(accountPoolProxyCredentialsTable, string(pool.ID), value)
		}
		clear(value)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO account_pool_proxy_credentials (pool_id, credential_value, sealed) VALUES (?, ?, 1)
ON CONFLICT(pool_id) DO UPDATE SET credential_value = excluded.credential_value, sealed = 1`, pool.ID, sealed)
		}
	}
	if err != nil {
		return record, err
	}
	record, err = readAccountPool(ctx, tx, pool.ID)
	if err != nil {
		return record, err
	}
	err = tx.Commit()
	return record, err
}

func (store *Store) ReplaceAccountPoolMembers(ctx context.Context, id contract.AccountPoolID, members []contract.AccountPoolMember, expected string) (record storage.AccountPoolRecord, err error) {
	if expected == "" {
		return record, fmt.Errorf("%w: expected ETag is required", storage.ErrInvalidArgument)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer rollbackOnError(tx, &err)
	record, err = readAccountPool(ctx, tx, id)
	if err != nil {
		return record, err
	}
	if record.ETag != expected {
		return record, storage.ErrPrecondition
	}
	seen := make(map[contract.ServiceID]bool, len(members))
	for _, member := range members {
		if err := member.Validate(); err != nil {
			return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
		}
		if member.PoolID != id || seen[member.ServiceID] {
			return record, fmt.Errorf("%w: invalid or duplicate membership", storage.ErrInvalidArgument)
		}
		seen[member.ServiceID] = true
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id = ?`, member.ServiceID).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return record, fmt.Errorf("%w: pool account does not exist", storage.ErrInvalidArgument)
			}
			return record, err
		}
		service, err := decodeServiceRecord(string(member.ServiceID), []byte(raw))
		if err != nil {
			return record, err
		}
		if err := record.Pool.ValidateMember(service.Service); err != nil {
			return record, fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
		}
		var owner string
		err = tx.QueryRowContext(ctx, `SELECT pool_id FROM account_pool_members WHERE service_id = ?`, member.ServiceID).Scan(&owner)
		if err == nil && owner != string(id) {
			return record, fmt.Errorf("%w: account already belongs to another pool", storage.ErrConflict)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return record, err
		}
	}
	change := storage.AccountPoolMutation{PoolID: id}
	currentMembers := make(map[contract.ServiceID]bool)
	for _, member := range record.Members {
		currentMembers[member.ServiceID] = true
		change.Accounts = append(change.Accounts, member.ServiceID)
		if !seen[member.ServiceID] {
			change.RequireIdle = append(change.RequireIdle, member.ServiceID)
		}
	}
	for _, member := range members {
		if !currentMembers[member.ServiceID] {
			change.Accounts = append(change.Accounts, member.ServiceID)
			change.RequireIdle = append(change.RequireIdle, member.ServiceID)
		}
	}
	release, err := store.beginPoolMutation(ctx, change)
	if err != nil {
		return record, err
	}
	defer func() {
		_ = tx.Rollback()
		release()
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_pool_members WHERE pool_id = ?`, id); err != nil {
		return record, err
	}
	for _, member := range members {
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_pool_members (pool_id, service_id, enabled, max_concurrent_requests) VALUES (?, ?, ?, ?)`, id, member.ServiceID, member.Enabled, member.MaxConcurrentRequests); err != nil {
			return record, err
		}
	}
	pool := record.Pool
	pool.UpdatedAt = store.now().UTC()
	raw, err := json.Marshal(pool)
	if err != nil {
		return record, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE account_pools SET document_json = ?, updated_at = ? WHERE id = ?`, string(raw), pool.UpdatedAt.Format(time.RFC3339Nano), id); err != nil {
		return record, err
	}
	record, err = readAccountPool(ctx, tx, id)
	if err != nil {
		return record, err
	}
	err = tx.Commit()
	return record, err
}

func (store *Store) DeleteAccountPool(ctx context.Context, id contract.AccountPoolID, expected string) (err error) {
	if expected == "" {
		return fmt.Errorf("%w: expected ETag is required", storage.ErrInvalidArgument)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnError(tx, &err)
	record, err := readAccountPool(ctx, tx, id)
	if err != nil {
		return err
	}
	if record.ETag != expected {
		return storage.ErrPrecondition
	}
	var referenced bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM local_access_tokens WHERE pool_id = ?)`, id).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: pool is referenced by access tokens", storage.ErrConflict)
	}
	change := storage.AccountPoolMutation{PoolID: id, Delete: true}
	for _, member := range record.Members {
		change.Accounts = append(change.Accounts, member.ServiceID)
		change.RequireIdle = append(change.RequireIdle, member.ServiceID)
	}
	release, err := store.beginPoolMutation(ctx, change)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
		release()
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_pools WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) getPoolProxyCredential(ctx context.Context, id contract.AccountPoolID) ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, secretstore.ErrNotFound
	}
	var value []byte
	var sealed bool
	err := store.db.QueryRowContext(ctx, `SELECT credential_value, sealed FROM account_pool_proxy_credentials WHERE pool_id = ?`, id).Scan(&value, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, secretstore.ErrNotFound
	}
	if err != nil {
		return nil, secretstore.ErrUnavailable
	}
	return store.openSecret(accountPoolProxyCredentialsTable, string(id), value, sealed)
}

func (store *Store) ListAccountQuotaCooldowns(ctx context.Context) ([]contract.AccountQuotaCooldown, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT document_json FROM account_quota_cooldowns ORDER BY service_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []contract.AccountQuotaCooldown{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var cooldown contract.AccountQuotaCooldown
		if err := json.Unmarshal([]byte(raw), &cooldown); err != nil {
			return nil, fmt.Errorf("%w: decode cooldown", storage.ErrInvalidRecord)
		}
		if err := cooldown.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", storage.ErrInvalidRecord, err)
		}
		result = append(result, cooldown)
	}
	return result, rows.Err()
}

func (store *Store) PutAccountQuotaCooldown(ctx context.Context, cooldown contract.AccountQuotaCooldown) error {
	if err := cooldown.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storage.ErrInvalidArgument, err)
	}
	raw, err := json.Marshal(cooldown)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO account_quota_cooldowns (service_id, document_json, observed_at) VALUES (?, ?, ?)
ON CONFLICT(service_id) DO UPDATE SET document_json = excluded.document_json, observed_at = excluded.observed_at
WHERE excluded.observed_at >= account_quota_cooldowns.observed_at`, cooldown.ServiceID, string(raw), bindingTime(cooldown.ObservedAt))
	return err
}

func (store *Store) DeleteAccountQuotaCooldown(ctx context.Context, id contract.ServiceID, observed contract.AccountQuotaCooldown) error {
	if observed.ServiceID != id {
		return fmt.Errorf("%w: cooldown identity mismatch", storage.ErrInvalidArgument)
	}
	raw, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, `DELETE FROM account_quota_cooldowns WHERE service_id = ? AND document_json = ?`, id, string(raw))
	return err
}
