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

// approvalHeader carries one base64 JSON signed approval per value. It may repeat, so
// an M-of-N quorum arrives on one request.
const approvalHeader = "X-Flynn-Approval"

// ActionFunc runs an admitted verb against a resolved resource. ctx carries the
// effective (intersected) grant, so any dispatch it performs is checked against the
// same authority. The result is JSON-encoded; an error maps to an HTTP status.
type ActionFunc func(ctx context.Context, p Principal, res resource.Resource) (any, error)

// ActionSpec declares an action subresource: a verb on a resource kind. This package
// owns the gate; the kill-switch and lifecycle code register verbs with [WithAction].
type ActionSpec struct {
	// Action is the dispatch action name checked against the effective grant, so local
	// and remote dispatch share one grant vocabulary.
	Action string
	// MinScope is the scope the verb requires. A token below it is refused before the
	// grant is consulted.
	MinScope Scope
	// Dangerous marks a verb that also needs a fresh, bound, single-use signed approval,
	// however broad the caller's scope or grant. With no approval verifier configured, a
	// Dangerous verb cannot run.
	Dangerous bool
	// Quorum is how many distinct signatures a Dangerous verb needs (M-of-N); zero means
	// one. It is ignored when Dangerous is false.
	Quorum int
	// Run performs the verb after admission.
	Run ActionFunc
}

// required returns the signatures the verb needs: zero when not dangerous, else
// Quorum floored at 1.
func (spec ActionSpec) required() int {
	if !spec.Dangerous {
		return 0
	}
	if spec.Quorum < 1 {
		return 1
	}
	return spec.Quorum
}

// handleAction is the grant gate, run after guardAction has enforced scope. The
// effective authority is the caller's grant intersected with this instance's local
// grant, so neither a narrowed remote grant nor a broad token can exceed what the
// instance admits locally. It audits exactly one outcome per action.
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
			// A missing target is not an access decision, so it is not audited.
			writeError(w, http.StatusNotFound, "not found")
			return
		}

		// AllowAll is the identity on either side: a local operator is bounded by the local
		// grant, and an unconstrained instance by the caller's grant.
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

		// A dangerous verb also needs a valid approval, even for an admin holding every
		// grant.
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

// actionResponse reports which verb ran on which resource, with the verb's result.
type actionResponse struct {
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	Result   any    `json:"result,omitempty"`
}

// approve runs the signed-approval gate. A non-dangerous verb passes. A dangerous verb
// needs a quorum of fresh, bound, single-use signatures for exactly this act, and
// fails closed when no verifier is configured. Each decision is recorded on the spine.
// It returns the refusal status, and ok only when the action may proceed.
func (s *Server) approve(ctx context.Context, p Principal, spec ActionSpec, verb, kind, name string, r *http.Request) (int, bool) {
	required := spec.required()
	if required == 0 {
		return 0, true // not a dangerous verb: scope and grant already decided it
	}
	if s.approvals == nil {
		// A dangerous verb registered without WithApprovals is a misconfiguration: refuse
		// rather than run it unverified.
		s.obs.Error(ctx, "controlplane: dangerous verb has no approval verifier configured",
			observe.String("verb", verb), observe.String("action", spec.Action))
		return http.StatusForbidden, false
	}

	// The approver must have signed this action, by this caller, on this target, on this
	// host. Detail pins the target so an approval to halt one resource cannot halt
	// another; Principal stops another caller replaying it. The verifier checks each
	// approval's window and nonce.
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

// parseApprovals decodes the request's X-Flynn-Approval values. A malformed value is
// skipped rather than failing the request: it does not count toward the quorum.
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

// recordApproval appends one approval decision to the spine. Like the access audit it
// is best-effort: an append failure is logged and does not change the outcome.
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

// resolve finds a resource by kind and name, trying the global scope first and then
// the others, as handleGet does. found is false, with no error, when no scope has it.
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
