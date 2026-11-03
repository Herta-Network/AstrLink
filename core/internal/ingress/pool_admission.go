package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/accountpool"
	"github.com/QuantumNous/astrlink/core/internal/claudeofficial"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
)

// installPoolAdmission runs after retry backoff and before each upstream
// attempt's privacy/authorization work. Release is called before the next
// backoff and once more at the request's final outcome.
func (handler *Handler) installPoolAdmission(request *http.Request, classified Request, candidates *[]endpoint.Resolved, schedule *recoverySchedule, body *requestBodySource, test *serviceTestExecution) func(bool) {
	if handler.poolRuntime == nil {
		return func(bool) {}
	}
	var permit *accountpool.Permit
	release := func(success bool) {
		if permit == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), time.Second)
		defer cancel()
		if err := handler.poolRuntime.Finish(ctx, permit, success); err != nil {
			logRequestRecordFailure(handler.recordLogger, "pool_quota_recovery", err)
		}
		permit = nil
	}
	principal, _ := accesstoken.PrincipalFromContext(request.Context())
	pooled := principal.RoutingScope == contract.RoutingScopePool
	session := recordSessionFromContext(request.Context())
	pinned, _ := requestedService(request)
	strict := classified.PreviousResponseID != "" || pinned != ""
	if turn := responsesWSTurnFromContext(request.Context()); turn != nil && turn.session.serviceID != "" {
		strict = true
	}
	key := ""
	if pooled && session != nil && session.channelBinding != nil {
		scope := session.channelBinding.scope
		// Two first requests can resolve different record session IDs before
		// either pending row exists. A client-authored conversation cursor
		// gives both admissions the same provisional identity; no prompt text
		// or cross-principal fingerprint becomes a routing key.
		cursor := classified.Conversation.SessionCursor
		if cursor == "" && classified.PreviousResponseID == "" && len(classified.Conversation.ExplicitCursors) > 0 {
			cursor = classified.Conversation.ExplicitCursors[0]
		}
		if cursor != "" {
			sum := sha256.Sum256([]byte(cursor))
			scope.SessionID = contract.SessionID("session_" + hex.EncodeToString(sum[:]))
		}
		encoded, _ := json.Marshal(struct {
			Scope    contract.ChannelBindingScope
			Revision int64
		}{scope, principal.RoutingRevision})
		key = string(encoded)
	}
	schedule.selectCandidate = func(ctx context.Context, proposed int) (int, error) {
		if !pooled && claudeofficial.Official((*candidates)[proposed].CanonicalService()) {
			return proposed, nil
		}
		for {
			forced := !pooled || strict || test != nil || schedule.nextRepair || schedule.counts[proposed] > 0
			proposedID := (*candidates)[proposed].CanonicalService().ID
			var lastView accountpool.View
			load := func(loadCtx context.Context) (accountpool.View, error) {
				baseIDs := []contract.ServiceID{proposedID}
				if !forced {
					baseIDs = baseIDs[:0]
					for _, candidate := range *candidates {
						baseIDs = append(baseIDs, candidate.CanonicalService().ID)
					}
				}
				view, err := handler.poolRuntime.View(loadCtx, principal, baseIDs, test != nil)
				if err != nil {
					return view, err
				}
				if view.Pool != nil && !view.Pool.Enabled && !view.Admin {
					return view, accountpool.ErrUnavailable
				}
				var fresh []endpoint.Resolved
				if test != nil {
					fresh = []endpoint.Resolved{(*candidates)[proposed]}
				} else {
					input := endpoint.ResolveRequest{Protocol: classified.Protocol, Model: classified.routingModel(), Streaming: classified.Streaming, Continuation: classified.PreviousResponseID != "", AllCandidates: true}
					if forced {
						input.Service = proposedID
					}
					var err error
					if resolver, ok := handler.resolver.(endpoint.CandidateResolver); ok {
						fresh, err = resolver.ResolveCandidates(loadCtx, input)
					} else {
						var single endpoint.Resolved
						single, err = handler.resolver.Resolve(loadCtx, input)
						if err == nil {
							fresh = []endpoint.Resolved{single}
						}
					}
					if err != nil {
						if errors.Is(err, accountpool.ErrScope) || errors.Is(err, accountpool.ErrNotBound) || errors.Is(err, accountpool.ErrModel) {
							return accountpool.View{}, err
						}
						allCooling, checked := true, false
						for _, id := range baseIDs {
							memberEnabled := view.Pool == nil
							for _, member := range view.Members {
								if member.ServiceID == id && member.Enabled {
									memberEnabled = true
									break
								}
							}
							if !memberEnabled {
								continue
							}
							checked = true
							if _, cooling := handler.poolRuntime.Scheduler.Cooldown(id); !cooling {
								allCooling = false
								break
							}
						}
						if strict {
							return accountpool.View{}, accountpool.ErrAffinity
						}
						if checked && allCooling {
							return accountpool.View{}, handler.poolRuntime.Scheduler.QuotaError(baseIDs)
						}
						return accountpool.View{}, err
					}
				}
				ids := []contract.ServiceID{}
				for _, current := range fresh {
					id := current.CanonicalService().ID
					index := -1
					for i, candidate := range *candidates {
						if candidate.CanonicalService().ID == id {
							index = i
							break
						}
					}
					if index < 0 {
						if forced {
							continue
						}
						index = len(*candidates)
						*candidates = append(*candidates, current)
						schedule.candidates = *candidates
						schedule.selected = append(schedule.selected, false)
						schedule.counts = append(schedule.counts, 0)
						schedule.retryable = append(schedule.retryable, false)
						schedule.excluded = append(schedule.excluded, false)
						schedule.readyAt = append(schedule.readyAt, time.Time{})
					}
					if schedule.excluded[index] || forced && index != proposed || !forced && index != proposed && schedule.selected[index] {
						continue
					}
					(*candidates)[index] = current
					ids = append(ids, id)
				}
				view.Candidates = ids
				view.StickyKey, view.Strict = key, strict
				if session != nil {
					view.SessionID = session.sessionID
				}
				if strict {
					view.Preferred = proposedID
				} else if session != nil && session.channelBinding != nil {
					view.Preferred = session.channelBinding.preferred
				}
				lastView = view
				return view, nil
			}
			bytes := body.size
			if bytes < 0 {
				bytes = accountpool.MaxWaitingBytes + 1
			}
			var err error
			permit, err = handler.poolRuntime.Scheduler.Acquire(ctx, accountpool.Request{Load: load, BodyBytes: bytes, ObserveWait: func(wait time.Duration) {
				if lastView.Pool != nil {
					scope := contract.RoutingScopeGlobal
					if pooled {
						scope = contract.RoutingScopePool
					}
					session.notePool(lastView.Pool.ID, lastView.Pool.Name, scope, wait)
				}
			}})
			if err == nil {
				if lastView.Pool != nil {
					scope := contract.RoutingScopeGlobal
					if pooled {
						scope = contract.RoutingScopePool
					}
					session.notePool(lastView.Pool.ID, lastView.Pool.Name, scope, 0)
				}
				if pooled && !strict {
					selection := contract.RoutingSelectionPoolBalance
					if permit.Sticky {
						selection = contract.RoutingSelectionSessionBinding
					}
					session.noteRoutingPin(selection, permit.ServiceID)
				}
				for i, candidate := range *candidates {
					if candidate.CanonicalService().ID == permit.ServiceID {
						return i, nil
					}
				}
				release(false)
				return 0, accountpool.ErrUnavailable
			}
			// A globally selected pooled account still obeys its capacity and
			// quota; normal global failover may then try the next provider.
			if !pooled && !strict && schedule.maySwitch && (errors.Is(err, accountpool.ErrBusy) || errors.Is(err, accountpool.ErrQuota) || errors.Is(err, accountpool.ErrUnavailable)) {
				schedule.selected[proposed], schedule.excluded[proposed] = true, true
				next := -1
				for i := range schedule.selected {
					if !schedule.selected[i] && !schedule.excluded[i] {
						next = i
						break
					}
				}
				if next >= 0 {
					proposed = next
					continue
				}
			}
			return 0, err
		}
	}
	return release
}
