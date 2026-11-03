package storage

import (
	"context"
	"fmt"
	"github.com/QuantumNous/astrlink/core/contract"
)

type AccountPoolRecord struct {
	Pool    contract.AccountPool
	Members []contract.AccountPoolMember
	ETag    string
}

type PoolProxyMutation struct {
	Present    bool
	Credential *contract.ProxyCredential
}

type AccountPoolStore interface {
	CreateAccountPool(context.Context, contract.AccountPool, PoolProxyMutation) (AccountPoolRecord, error)
	GetAccountPool(context.Context, contract.AccountPoolID) (AccountPoolRecord, error)
	ListAccountPools(context.Context) ([]AccountPoolRecord, error)
	UpdateAccountPool(context.Context, contract.AccountPool, PoolProxyMutation, string) (AccountPoolRecord, error)
	ReplaceAccountPoolMembers(context.Context, contract.AccountPoolID, []contract.AccountPoolMember, string) (AccountPoolRecord, error)
	DeleteAccountPool(context.Context, contract.AccountPoolID, string) error
	AccountPoolForService(context.Context, contract.ServiceID) (AccountPoolRecord, bool, error)
	AccountPoolMembership(context.Context, contract.ServiceID) (contract.AccountPool, contract.AccountPoolMember, bool, error)
}

type AccountQuotaStore interface {
	ListAccountQuotaCooldowns(context.Context) ([]contract.AccountQuotaCooldown, error)
	PutAccountQuotaCooldown(context.Context, contract.AccountQuotaCooldown) error
	DeleteAccountQuotaCooldown(context.Context, contract.ServiceID, contract.AccountQuotaCooldown) error
}

type AccountPoolMutation struct {
	Exclusive   bool
	PoolID      contract.AccountPoolID
	Accounts    []contract.ServiceID
	RequireIdle []contract.ServiceID
	Delete      bool
}

// AccountPoolObserver is installed during single-process Core composition.
// The short begin call fences new admissions; release must run after the
// transaction has committed or rolled back, never while a scheduler lock is held.
type AccountPoolObserver interface {
	BeginPoolMutation(context.Context, AccountPoolMutation) (func(), error)
	Changed()
}

type AccountPoolSyncChange struct {
	ID       contract.AccountPoolID
	Document []byte
	// Deleted detaches membership now. The sync engine removes the pool row
	// after applying token changes, so foreign-key protection remains active.
	Deleted bool
}

type AccountPoolSyncStore interface {
	SyncWriteAccountPools(context.Context, []AccountPoolSyncChange) error
}

// ErrPoolMembership requires detaching the account before deleting it or
// changing it to an execution mode that a pool cannot serve.
var ErrPoolMembership = fmt.Errorf("%w: account belongs to a pool", ErrConflict)

// ServiceDeletionGuard fences pool admission/membership while the control
// layer removes credentials, without holding a database transaction open.
type ServiceDeletionGuard interface {
	GuardServiceDeletion(context.Context, contract.ServiceID, string) (func(), error)
}
