package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/resource"
)

// ActionFunc executes an action verb against a resolved resource, once the gate has
// admitted it. It runs with the effective (intersected) capability grant already bound
// to ctx, so any further dispatch it performs is checked against the same authority.
// Its result is JSON-encoded into the response; an error is mapped to an HTTP status.
type ActionFunc func(ctx context.Context, p Principal, res resource.Resource) (any, error)

// ActionSpec declares an action subresource: a kube-style verb on a resource kind,
// distinct from a read. The control-plane owns the gate; the kill-switch and lifecycle
// layers own the actual verbs and register them with WithAction.
type ActionSpec struct {
	// Action is the dispatch action name the gate checks against the effective grant.
	// It is what authority the verb requires, named so the same grant vocabulary that
	// governs local dispatch governs remote action.
	Action string
	// MinScope is the coarse access level the verb requires (operator or admin). A
	// token below it is refused before the grant gate is even consulted.
	MinScope Scope
	// Dangerous marks a verb that the signed-approval gate must also clear (remote
	// dispatch, credential release, destructive/irreversible). It is a declarative
	// property here so a new dangerous verb cannot forget the gate; the approval check
	// itself is a separate layer that reads this flag.
	Dangerous bool
	// Run performs the verb after admission.
	Run ActionFunc
}

// handleAction is the action gate and the no-escalation enforcement point. The verb's
// scope is already enforced by guardAction; here the second, finer gate runs: the
// effective authority is the caller's verified grant INTERSECTED with this instance's
// own local grant, and the verb is admitted only if that intersection permits the
// action. A narrowed remote grant can therefore never act beyond what the target would
// do locally, and neither can a broad token exceed the instance's local ceiling. The
// single decision (allowed or denied) is audited here, so an action records exactly one
// outcome.
func (s *Server) handleAction(verb string, spec ActionSpec) func(http.ResponseWriter, *http.Request, Principal) {
	return func(w http.ResponseWriter, r *http.Request, p Principal) {
		ctx := r.Context()
		kind, name := r.PathValue("kind"), r.PathValue("name")

		res, found, err := s.resolve(ctx, kind, name)
		if err != nil {
			s.obs.Error(ctx, "controlplane: action resolve",
				observe.String("kind", kind), observe.String("verb", verb), observe.Err(err))
			writeError(w, http.StatusBadRequest, "cannot resolve "+kind+"/"+name)
			return
		}
		if !found {
			// A missing target is a 404, not an authz decision, so it is not audited as
			// an access outcome; nothing was acted on.
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		// The intersection IS the no-escalation rule. AllowAll on either side is the
		// identity, so a local operator (AllowAll) is bounded by the instance's local
		// grant, and a locally-unconstrained instance is bounded by the caller's grant.
		effective := p.Grant.Intersect(s.localGrant)
		gctx := capability.Into(ctx, effective)
		if err := s.admit.Admit(gctx, dispatch.Action{Name: spec.Action}); err != nil {
			s.audit(ctx, p, decisionDenied, spec.MinScope, verb, r)
			s.obs.Info(ctx, "controlplane: action denied by grant",
				observe.String("principal", p.ID), observe.String("verb", verb),
				observe.String("action", spec.Action))
			writeError(w, http.StatusForbidden, "action not permitted by grant")
			return
		}

		s.audit(ctx, p, decisionAllowed, spec.MinScope, verb, r)
		s.obs.Info(ctx, "controlplane: action",
			observe.String("principal", p.ID), observe.String("verb", verb))

		result, err := spec.Run(gctx, p, res)
		if err != nil {
			status := http.StatusInternalServerError
			if fault.Classify(err) == fault.Forbidden {
				status = http.StatusForbidden
			}
			s.obs.Error(ctx, "controlplane: action run",
				observe.String("verb", verb), observe.Err(err))
			writeError(w, status, "action "+verb+" failed")
			return
		}
		writeJSON(w, http.StatusOK, actionResponse{Verb: verb, Resource: res.Name, Result: result})
	}
}

// actionResponse is the envelope for an action result: which verb ran on which
// resource, and the verb's own result payload.
type actionResponse struct {
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Result   any    `json:"result,omitempty"`
}

// resolve finds a resource by kind and name, trying the global scope first (the
// unambiguous n=1 case) and then the other scopes, matching how handleGet and
// handleList resolve a name across scopes. It reports found=false (no error) when the
// name exists in no scope.
func (s *Server) resolve(ctx context.Context, kind, name string) (resource.Resource, bool, error) {
	res, err := s.store.Get(ctx, kind, resource.Scope{}, name)
	if err == nil {
		return res, true, nil
	}
	if !errors.Is(err, resource.ErrNotFound) {
		return resource.Resource{}, false, err
	}
	return s.getAcrossScopes(ctx, kind, name)
}
