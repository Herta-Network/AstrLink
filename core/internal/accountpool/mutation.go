package accountpool

import (
	"context"
	"fmt"
	"sync"

	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func (s *Scheduler) BeginPoolMutation(ctx context.Context, mutation storage.AccountPoolMutation) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if mutation.PoolID != "" && s.changingPools[mutation.PoolID] {
		return nil, fmt.Errorf("%w: account pool update in progress", storage.ErrConflict)
	}
	for _, id := range mutation.Accounts {
		if s.exclusiveAccounts[id] || mutation.Exclusive && s.changingAccounts[id] > 0 {
			return nil, fmt.Errorf("%w: account update in progress", storage.ErrConflict)
		}
	}
	for _, id := range mutation.RequireIdle {
		if s.active[id] > 0 {
			return nil, fmt.Errorf("%w: account has active requests", storage.ErrConflict)
		}
	}
	if mutation.Delete {
		for _, w := range s.waiters {
			if w.view.Pool != nil && w.view.Pool.ID == mutation.PoolID {
				return nil, fmt.Errorf("%w: pool has waiting requests", storage.ErrConflict)
			}
		}
	}
	if mutation.PoolID != "" {
		s.changingPools[mutation.PoolID] = true
	}
	for _, id := range mutation.Accounts {
		s.changingAccounts[id]++
		if mutation.Exclusive {
			s.exclusiveAccounts[id] = true
		}
	}
	s.epoch++
	s.signalLocked()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.changingPools, mutation.PoolID)
			for _, id := range mutation.Accounts {
				s.changingAccounts[id]--
				if mutation.Exclusive {
					delete(s.exclusiveAccounts, id)
				}
				if s.changingAccounts[id] == 0 {
					delete(s.changingAccounts, id)
				}
			}
			s.epoch++
			s.signalLocked()
		})
	}, nil
}
