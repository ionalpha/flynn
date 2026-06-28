package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// approvalHeader carries a signed approval for a dangerous verb. It may appear more
// than once, one base64-encoded JSON approval per value, so a quorum (M-of-N) is
// assembled from several independent approvers' signatures on one request.
const approvalHeader = "X-Flynn-Approval"

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
	// property so a new dangerous verb cannot forget the gate: a Dangerous verb fails
	// closed if no approval verifier is configured, and requires a fresh, bound,
	// single-use signature before it runs, regardless of how broad the caller's scope
	// or grant is.
	Dangerous bool
	// Quorum is how many distinct authorized signatures a dangerous verb requires
	// (M-of-N). Zero is treated as one for a Dangerous verb; it is ignored for a
	// non-dangerous verb.
	Quorum int
	// Run performs the verb after admission.
	Run ActionFunc
}

// required returns the number of approval signatures this verb needs: zero for a
// non-dangerous verb, or at least one for a dangerous one (Quorum, floored at 1).
func (spec ActionSpec) required() int {
	if !spec.Dangerous {
		return 0
	}
	if spec.Quorum < 1 {
		return 1
	}
	return spec.Quorum
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

		// The signed-approval gate: a dangerous verb needs a fresh, bound, single-use
		// approval on top of scope and grant, independent of how privileged the caller
		// is. It fails closed: an admin-scoped, fully-granted caller is still refused
		// without a valid approval, and a verb declared dangerous with no verifier
		// configured cannot run at all.
		if status, ok := s.approve(ctx, p, spec, verb, kind, name, r); !ok {
			s.audit(ctx, p, decisionDenied, spec.MinScope, verb, r)
			writeError(w, status, "action not authorized: a valid approval is required")
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

// approve runs the signed-approval gate for a verb. A non-dangerous verb needs no
// approval and passes through. A dangerous verb requires a quorum of fresh, bound,
// single-use signatures: it fails closed when no verifier is configured (a dangerous
// verb cannot run unverified) and otherwise admits only when the presented approvals
// authorize exactly this act. The decision (who signed, granted or not) is recorded on
// the spine as the non-repudiable approval record. It returns the HTTP status to use on
// refusal and ok=true only when the action may proceed.
func (s *Server) approve(ctx context.Context, p Principal, spec ActionSpec, verb, kind, name string, r *http.Request) (int, bool) {
	required := spec.required()
	if required == 0 {
		return 0, true // not a dangerous verb: scope and grant already decided it
	}
	if s.approvals == nil {
		// Fail closed: a verb declared dangerous must never run where its second factor
		// cannot be checked. This is a misconfiguration (a dangerous verb registered
		// without WithApprovals), refused rather than silently downgraded.
		s.obs.Error(ctx, "controlplane: dangerous verb has no approval verifier configured",
			observe.String("verb", verb), observe.String("action", spec.Action))
		return http.StatusForbidden, false
	}

	// The binding the approver must have signed: this exact action, on this caller, for
	// this target resource, valid on this host. Detail pins the target so an approval to
	// halt one resource cannot halt another; Principal pins the caller so an approval
	// granted to one cannot be replayed by another. The window and nonce live on each
	// presented approval and are checked by the verifier.
	want := approval.Envelope{
		Action:    spec.Action,
		Principal: p.ID,
		Detail:    kind + "/" + name,
		Host:      s.approvalHost,
	}
	presented := parseApprovals(r)
	dec, err := s.approvals.Check(ctx, want, presented, required)
	if err != nil {
		s.obs.Error(ctx, "controlplane: approval check error",
			observe.String("verb", verb), observe.Err(err))
		return http.StatusForbidden, false
	}
	s.recordApproval(ctx, p, verb, dec)
	if !dec.Granted {
		s.obs.Info(ctx, "controlplane: action denied by approval",
			observe.String("principal", p.ID), observe.String("verb", verb),
			observe.String("reason", dec.Reason))
		return http.StatusForbidden, false
	}
	return 0, true
}

// parseApprovals decodes the signed approvals presented on the request, one base64 JSON
// approval per X-Flynn-Approval header value. A malformed value is skipped rather than
// failing the request: the verifier decides authorization on what validly decodes, and
// a junk header simply does not count toward the quorum.
func parseApprovals(r *http.Request) []approval.Approval {
	values := r.Header.Values(approvalHeader)
	out := make([]approval.Approval, 0, len(values))
	for _, v := range values {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			continue
		}
		var a approval.Approval
		if err := json.Unmarshal(raw, &a); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out
}

// recordApproval lands one approval decision on the spine: the non-repudiable record of
// who authorized (or failed to authorize) a dangerous verb. It is best-effort, like the
// access audit: a failure to append is logged but never admits or fails the request.
func (s *Server) recordApproval(ctx context.Context, p Principal, verb string, dec approval.Decision) {
	if s.log == nil {
		return
	}
	if _, err := s.log.Append(ctx, spine.AppendInput{
		Stream:    AuditStream,
		Type:      EvApproval,
		Actor:     spine.ActorHuman,
		Principal: p.ID,
		Payload: map[string]any{
			"action":  dec.Envelope.Action,
			"verb":    verb,
			"detail":  dec.Envelope.Detail,
			"granted": dec.Granted,
			"reason":  dec.Reason,
			"keyIds":  dec.KeyIDs,
		},
	}); err != nil {
		s.obs.Error(ctx, "controlplane: approval audit append failed", observe.Err(err))
	}
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
