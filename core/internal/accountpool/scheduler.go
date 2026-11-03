package accountpool

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

const MaxWaitingRequests = 128
const MaxWaitingBytes int64 = 128 << 20

// View is reloaded outside the scheduler lock whenever admission wakes. Its
// candidates must already satisfy model, protocol, credentials and key scope.
type View struct {
	Pool          *contract.AccountPool
	Members       []contract.AccountPoolMember
	Candidates    []contract.ServiceID
	StickyKey     string
	SessionID     contract.SessionID
	Preferred     contract.ServiceID
	Strict        bool
	PreserveOrder bool
	Admin         bool
}

type Request struct {
	Load        func(context.Context) (View, error)
	BodyBytes   int64
	ObserveWait func(time.Duration)
}

type stickyBinding struct {
	sessions    map[contract.SessionID]int
	lastSession contract.SessionID
	service     contract.ServiceID
	refs        int
	success     bool
	expires     time.Time
}

type cooldownState struct {
	value        contract.AccountQuotaCooldown
	probeAllowed bool
	probing      bool
}

type waiter struct {
	ctx      context.Context
	view     View
	bytes    int64
	deadline time.Time
	started  time.Time
}

type Scheduler struct {
	mu                sync.Mutex
	changed           chan struct{}
	epoch             uint64
	active            map[contract.ServiceID]int
	lastGranted       map[contract.ServiceID]uint64
	sequence          uint64
	bindings          map[string]*stickyBinding
	cooldowns         map[contract.ServiceID]*cooldownState
	waiters           []*waiter
	waitingBytes      int64
	changingPools     map[contract.AccountPoolID]bool
	changingAccounts  map[contract.ServiceID]int
	exclusiveAccounts map[contract.ServiceID]bool
	now               func() time.Time
}

func NewScheduler() *Scheduler {
	return &Scheduler{changed: make(chan struct{}), active: map[contract.ServiceID]int{}, lastGranted: map[contract.ServiceID]uint64{}, bindings: map[string]*stickyBinding{}, cooldowns: map[contract.ServiceID]*cooldownState{}, changingPools: map[contract.AccountPoolID]bool{}, changingAccounts: map[contract.ServiceID]int{}, exclusiveAccounts: map[contract.ServiceID]bool{}, now: time.Now}
}

type Permit struct {
	ServiceID contract.ServiceID
	Wait      time.Duration
	Probe     bool
	Sticky    bool
	once      sync.Once
	scheduler *Scheduler
	view      View
	binding   *stickyBinding
	cooldown  *cooldownState
}

// Finish releases a slot once. A successful half-open probe returns the exact
// observation to conditionally remove from durable storage after unlocking.
func (permit *Permit) Finish(success bool) *contract.AccountQuotaCooldown {
	var recovered *contract.AccountQuotaCooldown
	permit.once.Do(func() {
		s := permit.scheduler
		s.mu.Lock()
		defer s.mu.Unlock()
		s.active[permit.ServiceID]--
		if s.active[permit.ServiceID] == 0 {
			delete(s.active, permit.ServiceID)
		}
		if binding := permit.binding; binding != nil {
			binding.refs--
			if permit.view.SessionID != "" {
				binding.sessions[permit.view.SessionID]--
				if binding.sessions[permit.view.SessionID] == 0 {
					delete(binding.sessions, permit.view.SessionID)
				}
			}
			if s.bindings[permit.view.StickyKey] == binding {
				if success {
					binding.success = true
					binding.lastSession = permit.view.SessionID
					binding.expires = s.now().Add(time.Duration(permit.view.Pool.Sticky.TTLSeconds) * time.Second)
				} else if binding.refs == 0 && !binding.success {
					delete(s.bindings, permit.view.StickyKey)
				}
			}
			if len(s.bindings) > 10000 && binding.refs == 0 && s.bindings[permit.view.StickyKey] == binding {
				delete(s.bindings, permit.view.StickyKey)
			}
		}
		if permit.Probe && s.cooldowns[permit.ServiceID] == permit.cooldown {
			permit.cooldown.probing = false
			permit.cooldown.probeAllowed = false
			if success {
				value := permit.cooldown.value
				recovered = &value
				delete(s.cooldowns, permit.ServiceID)
			}
		}
		s.signalLocked()
	})
	return recovered
}

func (s *Scheduler) signalLocked() { close(s.changed); s.changed = make(chan struct{}) }

// Changed invalidates views loaded before a committed configuration change.
// The store calls it before returning a successful mutation to its caller.
func (s *Scheduler) Changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
	s.signalLocked()
}

func (s *Scheduler) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Scheduler) Counts(service contract.ServiceID, pool contract.AccountPoolID) (active, waiting int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active = s.active[service]
	for _, w := range s.waiters {
		if w.view.Pool != nil && w.view.Pool.ID == pool {
			waiting++
		}
	}
	return
}

func (s *Scheduler) Acquire(ctx context.Context, request Request) (*Permit, error) {
	if request.Load == nil || request.BodyBytes < 0 {
		return nil, ErrUnavailable
	}
	var queued *waiter
	var waitingSince time.Time
	defer func() {
		if queued != nil {
			s.mu.Lock()
			s.removeWaiterLocked(queued)
			s.signalLocked()
			s.mu.Unlock()
		}
		if !waitingSince.IsZero() && request.ObserveWait != nil {
			request.ObserveWait(time.Since(waitingSince))
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		epoch := s.epoch
		s.mu.Unlock()
		loadContext := ctx
		cancelLoad := func() {}
		if queued != nil {
			if !queued.deadline.After(time.Now()) {
				return nil, ErrBusy
			}
			loadContext, cancelLoad = context.WithDeadline(ctx, queued.deadline)
		}
		view, err := request.Load(loadContext)
		cancelLoad()
		if err != nil && queued != nil && ctx.Err() == nil && !queued.deadline.After(time.Now()) {
			return nil, ErrBusy
		}
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if epoch != s.epoch {
			s.mu.Unlock()
			continue
		}
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if queued != nil {
			queued.view = view
		}
		service, err := s.chooseLocked(view, queued)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if service != "" {
			permit := s.grantLocked(service, view)
			if queued != nil {
				permit.Wait = time.Since(queued.started)
				s.removeWaiterLocked(queued)
				queued = nil
			}
			s.mu.Unlock()
			return permit, nil
		}
		if queued == nil {
			queue := contract.AccountPoolQueue{MaxWaitingRequests: contract.DefaultPoolWaiting, TimeoutMS: contract.DefaultPoolWaitMS}
			poolID := contract.AccountPoolID("")
			if view.Pool != nil {
				queue = view.Pool.Queue
				poolID = view.Pool.ID
			}
			if queue.MaxWaitingRequests == 0 {
				s.mu.Unlock()
				return nil, ErrBusy
			}
			waiting := 0
			for _, w := range s.waiters {
				other := contract.AccountPoolID("")
				if w.view.Pool != nil {
					other = w.view.Pool.ID
				}
				if other == poolID {
					waiting++
				}
			}
			if waiting >= queue.MaxWaitingRequests || len(s.waiters) >= MaxWaitingRequests || request.BodyBytes > MaxWaitingBytes-s.waitingBytes {
				s.mu.Unlock()
				return nil, ErrQueueFull
			}
			now := time.Now()
			queued = &waiter{ctx: ctx, view: view, bytes: request.BodyBytes, started: now, deadline: now.Add(time.Duration(queue.TimeoutMS) * time.Millisecond)}
			waitingSince = now
			s.waiters = append(s.waiters, queued)
			s.waitingBytes += queued.bytes
			s.signalLocked()
		}
		notify := s.changed
		remaining := time.Until(queued.deadline)
		s.mu.Unlock()
		if remaining <= 0 {
			return nil, ErrBusy
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrBusy
		case <-notify:
			timer.Stop()
		}
	}
}

func (s *Scheduler) removeWaiterLocked(target *waiter) {
	for i, w := range s.waiters {
		if w == target {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			s.waitingBytes -= w.bytes
			return
		}
	}
}

func (s *Scheduler) memberLimit(view View, id contract.ServiceID) (int, bool) {
	if view.Pool == nil {
		return 0, true
	}
	for _, member := range view.Members {
		if member.ServiceID == id && member.PoolID == view.Pool.ID {
			return member.MaxConcurrentRequests, (member.Enabled || view.Admin) && member.MaxConcurrentRequests > 0
		}
	}
	return 0, false
}

func (s *Scheduler) quotaBlocks(id contract.ServiceID) bool {
	state := s.cooldowns[id]
	return state != nil && (!state.probeAllowed || state.probing)
}

func (s *Scheduler) preferredLocked(view View) contract.ServiceID {
	if view.Strict {
		return view.Preferred
	}
	if view.Pool == nil || !view.Pool.Sticky.Enabled || view.StickyKey == "" {
		return ""
	}
	if binding := s.bindings[view.StickyKey]; binding != nil {
		if binding.refs > 0 || binding.expires.After(s.now()) {
			return binding.service
		}
		delete(s.bindings, view.StickyKey)
	}
	return view.Preferred
}

func (s *Scheduler) waitingBeforeLocked(id contract.ServiceID, current *waiter) bool {
	for _, w := range s.waiters {
		if w == current {
			break
		}
		if w.ctx.Err() != nil || !w.deadline.After(time.Now()) {
			continue
		}
		preferred := s.preferredLocked(w.view)
		if preferred != "" && preferred != id {
			if _, valid := s.memberLimit(w.view, preferred); valid && !s.quotaBlocks(preferred) {
				continue
			}
		}
		for _, candidate := range w.view.Candidates {
			if candidate == id {
				return true
			}
		}
	}
	return false
}

func (s *Scheduler) chooseLocked(view View, queued *waiter) (contract.ServiceID, error) {
	if view.Pool != nil && !view.Pool.Enabled && !view.Admin {
		return "", ErrUnavailable
	}
	if len(view.Candidates) == 0 {
		if view.Strict {
			return "", ErrAffinity
		}
		return "", ErrUnavailable
	}
	if view.Pool != nil && s.changingPools[view.Pool.ID] {
		return "", nil
	}
	preferred := s.preferredLocked(view)
	eligible := []contract.ServiceID{}
	quotaCount := 0
	for _, candidate := range view.Candidates {
		if _, valid := s.memberLimit(view, candidate); !valid {
			continue
		}
		if !view.Admin && s.quotaBlocks(candidate) {
			quotaCount++
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		if view.Strict {
			return "", ErrAffinity
		}
		if quotaCount > 0 {
			return "", s.quotaErrorLocked(view.Candidates)
		}
		return "", ErrUnavailable
	}
	preferredEligible := false
	for _, id := range eligible {
		preferredEligible = preferredEligible || id == preferred
	}
	if preferredEligible {
		eligible = []contract.ServiceID{preferred}
	} else if view.Strict {
		return "", ErrAffinity
	}
	var chosen contract.ServiceID
	for _, id := range eligible {
		limit, _ := s.memberLimit(view, id)
		if s.changingAccounts[id] > 0 || limit > 0 && s.active[id] >= limit || s.waitingBeforeLocked(id, queued) {
			continue
		}
		if chosen == "" {
			chosen = id
			if view.PreserveOrder {
				break
			}
			continue
		}
		chosenLimit, _ := s.memberLimit(view, chosen)
		if limit == 0 {
			limit = contract.MaxAccountConcurrency
		}
		if chosenLimit == 0 {
			chosenLimit = contract.MaxAccountConcurrency
		}
		left, right := int64(s.active[id])*int64(chosenLimit), int64(s.active[chosen])*int64(limit)
		if left < right || left == right && s.lastGranted[id] < s.lastGranted[chosen] {
			chosen = id
		}
	}
	return chosen, nil
}

func (s *Scheduler) grantLocked(id contract.ServiceID, view View) *Permit {
	s.active[id]++
	s.sequence++
	s.lastGranted[id] = s.sequence
	permit := &Permit{ServiceID: id, scheduler: s, view: view}
	permit.Sticky = !view.Strict && s.preferredLocked(view) == id
	if view.Pool != nil && view.Pool.Sticky.Enabled && view.StickyKey != "" {
		if len(s.bindings) >= 10000 {
			for key, cached := range s.bindings {
				if cached.refs == 0 {
					delete(s.bindings, key)
					if len(s.bindings) < 10000 {
						break
					}
				}
			}
		}
		binding := s.bindings[view.StickyKey]
		if binding == nil || binding.service != id {
			binding = &stickyBinding{service: id, sessions: map[contract.SessionID]int{}}
			s.bindings[view.StickyKey] = binding
		}
		binding.refs++
		if view.SessionID != "" {
			binding.sessions[view.SessionID]++
		}
		permit.binding = binding
	}
	if state := s.cooldowns[id]; state != nil && state.probeAllowed && !state.probing && !view.Admin {
		state.probing = true
		permit.Probe, permit.cooldown = true, state
	}
	return permit
}

func (s *Scheduler) SetCooldown(value contract.AccountQuotaCooldown) error {
	if err := value.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.cooldowns[value.ServiceID]; current != nil && current.value.ObservedAt.After(value.ObservedAt) {
		return nil
	}
	if current := s.cooldowns[value.ServiceID]; current != nil && reflect.DeepEqual(current.value, value) {
		return nil
	}
	s.cooldowns[value.ServiceID] = &cooldownState{value: cloneCooldown(value)}
	s.signalLocked()
	return nil
}

func (s *Scheduler) ClearCooldown(observed contract.AccountQuotaCooldown) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.cooldowns[observed.ServiceID]
	if current == nil || !reflect.DeepEqual(current.value, observed) {
		return false
	}
	delete(s.cooldowns, observed.ServiceID)
	s.signalLocked()
	return true
}

func (s *Scheduler) AllowProbe(id contract.ServiceID, manual bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.cooldowns[id]; state != nil && !state.probing {
		reset := state.value.RetryAt()
		if !manual && (reset.IsZero() || reset.After(s.now())) {
			return false
		}
		state.probeAllowed = true
		s.signalLocked()
		return true
	}
	return false
}

func (s *Scheduler) Cooldown(id contract.ServiceID) (contract.AccountQuotaCooldown, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.cooldowns[id]; state != nil {
		return cloneCooldown(state.value), true
	}
	return contract.AccountQuotaCooldown{}, false
}

func cloneCooldown(value contract.AccountQuotaCooldown) contract.AccountQuotaCooldown {
	value.Windows = append([]contract.AccountQuotaWindow(nil), value.Windows...)
	for i := range value.Windows {
		if value.Windows[i].ResetAt != nil {
			at := *value.Windows[i].ResetAt
			value.Windows[i].ResetAt = &at
		}
		if value.Windows[i].LimitWindowSeconds != nil {
			duration := *value.Windows[i].LimitWindowSeconds
			value.Windows[i].LimitWindowSeconds = &duration
		}
	}
	return value
}

func (s *Scheduler) QuotaError(candidates []contract.ServiceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quotaErrorLocked(candidates)
}
func (s *Scheduler) quotaErrorLocked(candidates []contract.ServiceID) error {
	var earliest time.Time
	for _, id := range candidates {
		if state := s.cooldowns[id]; state != nil {
			at := state.value.RetryAt()
			if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
	}
	return &QuotaExhausted{RetryAt: earliest}
}

// ForgetSession mirrors the explicit persisted binding release without
// changing active permits. Old completions cannot recreate the cached entry.
func (s *Scheduler) ForgetSession(id contract.SessionID) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, binding := range s.bindings {
		if binding.lastSession == id || binding.sessions[id] > 0 {
			delete(s.bindings, key)
		}
	}
	s.epoch++
	s.signalLocked()
}
