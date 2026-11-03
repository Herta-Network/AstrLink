package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func (store *Store) syncAccountPools(ctx context.Context) ([]storage.SyncLocalEntity, error) {
	records, err := store.ListAccountPools(ctx)
	if err != nil {
		return nil, err
	}
	entities := make([]storage.SyncLocalEntity, 0, len(records))
	for _, record := range records {
		raw, err := json.Marshal(contract.AccountPoolDocument{AccountPool: record.Pool, Members: record.Members})
		if err != nil {
			return nil, err
		}
		entities = append(entities, storage.SyncLocalEntity{LocalID: string(record.Pool.ID), Document: raw, UpdatedAt: record.Pool.UpdatedAt})
	}
	return entities, nil
}

// Pool metadata and membership form one sync entity and one SQL transaction.
// Missing dependencies fail the sync; they never turn a pool key global.
func (store *Store) syncWriteAccountPool(ctx context.Context, id string, document []byte) error {
	return store.SyncWriteAccountPools(ctx, []storage.AccountPoolSyncChange{{ID: contract.AccountPoolID(id), Document: document}})
}

func (store *Store) SyncWriteAccountPools(ctx context.Context, changes []storage.AccountPoolSyncChange) (err error) {
	desired := map[contract.AccountPoolID]contract.AccountPoolDocument{}
	deleted := map[contract.AccountPoolID]bool{}
	ids := []contract.AccountPoolID{}
	owners := map[contract.ServiceID]contract.AccountPoolID{}
	for _, change := range changes {
		if change.ID.Validate() != nil {
			return fmt.Errorf("%w: invalid synced pool id", storage.ErrConflict)
		}
		if _, duplicate := desired[change.ID]; duplicate || deleted[change.ID] {
			return fmt.Errorf("%w: duplicate synced pool", storage.ErrConflict)
		}
		ids = append(ids, change.ID)
		if change.Deleted {
			deleted[change.ID] = true
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(change.Document))
		decoder.DisallowUnknownFields()
		var incoming contract.AccountPoolDocument
		if decoder.Decode(&incoming) != nil || incoming.ID != change.ID || incoming.Validate() != nil || incoming.Members == nil {
			return fmt.Errorf("%w: invalid synced pool document", storage.ErrConflict)
		}
		sort.Slice(incoming.Members, func(i, j int) bool { return incoming.Members[i].ServiceID < incoming.Members[j].ServiceID })
		for _, member := range incoming.Members {
			if member.Validate() != nil || member.PoolID != change.ID || owners[member.ServiceID] != "" {
				return fmt.Errorf("%w: invalid or conflicting synced membership", storage.ErrConflict)
			}
			owners[member.ServiceID] = change.ID
		}
		desired[change.ID] = incoming
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackOnError(tx, &err)
	current := map[contract.AccountPoolID]storage.AccountPoolRecord{}
	changed := []contract.AccountPoolID{}
	for _, id := range ids {
		record, readErr := readAccountPool(ctx, tx, id)
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return readErr
		}
		if deleted[id] {
			if readErr == nil && len(record.Members) > 0 {
				current[id] = record
				changed = append(changed, id)
			}
			continue
		}
		incoming := desired[id]
		if readErr == nil {
			if record.Pool.Provider != incoming.Provider {
				return fmt.Errorf("%w: pool provider cannot change", storage.ErrConflict)
			}
			existing, _ := json.Marshal(contract.AccountPoolDocument{AccountPool: record.Pool, Members: record.Members})
			wanted, _ := json.Marshal(incoming)
			if bytes.Equal(existing, wanted) {
				continue
			}
		}
		current[id] = record
		changed = append(changed, id)
		for _, member := range incoming.Members {
			var raw string
			if err := tx.QueryRowContext(ctx, `SELECT document_json FROM services WHERE id = ?`, member.ServiceID).Scan(&raw); err != nil {
				return fmt.Errorf("%w: synced pool account unavailable", storage.ErrConflict)
			}
			service, err := decodeServiceRecord(string(member.ServiceID), []byte(raw))
			if err != nil || incoming.ValidateMember(service.Service) != nil {
				return fmt.Errorf("%w: synced account provider or mode unsupported", storage.ErrConflict)
			}
			var owner contract.AccountPoolID
			err = tx.QueryRowContext(ctx, `SELECT pool_id FROM account_pool_members WHERE service_id = ?`, member.ServiceID).Scan(&owner)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && owner != id {
				if _, moving := desired[owner]; !moving && !deleted[owner] {
					return fmt.Errorf("%w: account belongs to a pool outside this sync", storage.ErrConflict)
				}
			}
		}
	}
	releases := []func(){}
	defer func() {
		_ = tx.Rollback()
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	for _, id := range changed {
		change := storage.AccountPoolMutation{PoolID: id}
		held := map[contract.ServiceID]bool{}
		for _, member := range current[id].Members {
			held[member.ServiceID] = true
			change.Accounts = append(change.Accounts, member.ServiceID)
			if owners[member.ServiceID] != id {
				change.RequireIdle = append(change.RequireIdle, member.ServiceID)
			}
		}
		for _, member := range desired[id].Members {
			if !held[member.ServiceID] {
				change.Accounts = append(change.Accounts, member.ServiceID)
				change.RequireIdle = append(change.RequireIdle, member.ServiceID)
			}
		}
		release, err := store.beginPoolMutation(ctx, change)
		if err != nil {
			return err
		}
		releases = append(releases, release)
	}
	for _, id := range changed {
		if !deleted[id] {
			incoming := desired[id]
			if incoming.CreatedAt.IsZero() {
				incoming.CreatedAt = store.now().UTC()
			}
			if incoming.UpdatedAt.IsZero() {
				incoming.UpdatedAt = store.now().UTC()
			}
			raw, err := json.Marshal(incoming.AccountPool)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO account_pools(id,document_json,created_at,updated_at) VALUES (?,?,?,?) ON CONFLICT(id) DO UPDATE SET document_json=excluded.document_json,updated_at=excluded.updated_at`, id, string(raw), incoming.CreatedAt.Format(time.RFC3339Nano), incoming.UpdatedAt.Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM account_pool_members WHERE pool_id = ?`, id); err != nil {
			return err
		}
	}
	for _, id := range changed {
		if !deleted[id] {
			for _, member := range desired[id].Members {
				if _, err = tx.ExecContext(ctx, `INSERT INTO account_pool_members(pool_id,service_id,enabled,max_concurrent_requests) VALUES (?,?,?,?)`, id, member.ServiceID, member.Enabled, member.MaxConcurrentRequests); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func bindPoolCredentialRefs(ctx context.Context, tx *sql.Tx) error {
	records, err := readAccountPools(ctx, tx, "")
	if err != nil {
		return err
	}
	for _, record := range records {
		pool := record.Pool
		if pool.Proxy == nil || pool.Proxy.Mode != "custom" {
			continue
		}
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_pool_proxy_credentials WHERE pool_id = ?)`, pool.ID).Scan(&present); err != nil {
			return err
		}
		// Keep an expected-but-missing reference: resolving it must fail closed,
		// not silently try a SOCKS proxy without its required authentication.
		if !present || pool.Proxy.CredentialRef != "" {
			continue
		}
		pool.Proxy.CredentialRef = "local://pool-proxy/" + string(pool.ID)
		raw, err := json.Marshal(pool)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_pools SET document_json = ? WHERE id = ?`, string(raw), pool.ID); err != nil {
			return err
		}
	}
	return nil
}
