package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// defaultWatchPoll is how often a watch re-reads the resource stream. The store does
// not push a wake, so this poll bounds latency; a read past the cursor returns only
// new events, so it is cheap.
const defaultWatchPoll = 250 * time.Millisecond

// Server is the control-plane HTTP API over a resource store: get, list and watch
// for every registered kind, plus action verbs behind a second gate, the caller's
// grant intersected with this instance's local grant.
type Server struct {
	store resource.Store
	log   spine.Log // the resource stream, tailed for watch
	auth  Authenticator
	obs   observe.Logger
	poll  time.Duration

	// localGrant is the ceiling every remote caller's grant is intersected with;
	// AllowAll by default. A host narrows it to bound what any token can do here.
	localGrant capability.Grant
	// admit is the capability waist the action gate runs an admitted verb through,
	// reading the effective (intersected) grant from the request context.
	admit dispatch.Admitter
	// actions maps a verb to its spec, registered with WithAction.
	actions map[string]ActionSpec

	// approvals verifies the signed approvals a dangerous verb needs; nil means a
	// dangerous verb cannot run. approvalHost is the host id an approval must be bound
	// to, so an approval minted for one host cannot authorize another.
	approvals    *approval.Verifier
	approvalHost string
}

// Option configures a Server.
type Option func(*Server)

// WithLogger sets the logger used for audit and errors (default: a discard logger).
func WithLogger(l observe.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.obs = l
		}
	}
}

// WithWatchPoll overrides the watch poll interval. A non-positive value is ignored.
func WithWatchPoll(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithLocalGrant sets the instance's local grant, the ceiling every remote caller's
// grant is intersected with. The default is AllowAll.
func WithLocalGrant(g capability.Grant) Option {
	return func(s *Server) { s.localGrant = g }
}

// WithAdmitter replaces the admitter the action gate uses (default
// capability.Admitter). It exists for tests.
func WithAdmitter(a dispatch.Admitter) Option {
	return func(s *Server) {
		if a != nil {
			s.admit = a
		}
	}
}

// WithApprovals sets the verifier for dangerous verbs and the host id an approval
// must be bound to, which should match the verifier's own host. Without it a
// Dangerous verb cannot run.
func WithApprovals(v *approval.Verifier, host string) Option {
	return func(s *Server) {
		s.approvals = v
		s.approvalHost = host
	}
}

// WithAction registers verb at POST /v1/{kind}/{name}/<verb>. It refuses a token
// below spec.MinScope and admits only when the caller's grant intersected with the
// local grant permits spec.Action. A spec with an empty Action or nil Run is ignored,
// so a half-declared action cannot open an ungated route.
func WithAction(verb string, spec ActionSpec) Option {
	return func(s *Server) {
		if verb == "" || spec.Action == "" || spec.Run == nil {
			return
		}
		s.actions[verb] = spec
	}
}

// NewServer builds the API over store, tailing log (the store's resource stream) for
// watch. A nil auth denies every request, so an open API cannot be created by
// omission.
func NewServer(store resource.Store, log spine.Log, auth Authenticator, opts ...Option) *Server {
	if auth == nil {
		auth = DenyAll{}
	}
	s := &Server{
		store:      store,
		log:        log,
		auth:       auth,
		obs:        observe.Default().Log,
		poll:       defaultWatchPoll,
		localGrant: capability.AllowAll(),
		admit:      capability.Admitter{},
		actions:    make(map[string]ActionSpec),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Handler returns the HTTP handler for the API. The watch route is registered
// before the by-name route so the literal "watch" segment wins over the {name}
// wildcard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/{kind}/watch", s.guard(ScopeRead, s.handleWatch))
	mux.HandleFunc("GET /v1/{kind}/{name}", s.guard(ScopeRead, s.handleGet))
	mux.HandleFunc("GET /v1/{kind}", s.guard(ScopeRead, s.handleList))
	// Action verbs are POSTs on a named resource, so they never collide with the GET read
	// routes.
	for verb, spec := range s.actions {
		mux.HandleFunc("POST /v1/{kind}/{name}/"+verb, s.guardAction(spec, verb, s.handleAction(verb, spec)))
	}
	return mux
}

// The audit stream and event types. Every authenticated request records its decision
// on AuditStream as an EvAccess event, so access history is a replayable fold over
// the spine.
const (
	// AuditStream is the spine stream every control-plane access decision is recorded
	// on, separate from the resource stream the watch tails.
	AuditStream = "controlplane.audit"
	// EvAccess is the event type of one access decision.
	EvAccess = "controlplane.access"
	// EvApproval is one signed-approval decision for a dangerous verb: the signing key
	// ids, the bound action and target, and whether it was granted.
	EvApproval = "controlplane.approval"
)

// Access decisions recorded in an audit event's payload under "decision".
const (
	decisionAllowed         = "allowed"
	decisionForbidden       = "forbidden"
	decisionUnauthenticated = "unauthenticated"
	// decisionDenied is a request that passed scope but failed the grant gate. It is
	// kept apart from decisionForbidden so an audit reader can tell under-scoping from a
	// capability denial.
	decisionDenied = "denied"
)

// authorize authenticates r and enforces required. On false the error response is
// written and the attempt audited. action is the verb attempted, "" for a read.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, required Scope, action string) (Principal, bool) {
	p, err := s.auth.Authenticate(r)
	if err != nil {
		s.audit(r.Context(), Principal{}, decisionUnauthenticated, required, action, r)
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return Principal{}, false
	}
	if !p.Scope.Allows(required) {
		s.audit(r.Context(), p, decisionForbidden, required, action, r)
		s.obs.Info(r.Context(), "controlplane: forbidden",
			observe.String("principal", p.ID), observe.String("scope", p.Scope.String()),
			observe.String("path", r.URL.Path))
		writeError(w, http.StatusForbidden, "insufficient scope")
		return Principal{}, false
	}
	return p, true
}

// guard authenticates, enforces the minimum scope and audits every outcome, failures
// included, since a refused attempt is security-relevant: 401 when unauthenticated,
// 403 when under-scoped. A read is fully decided here, so it is recorded allowed
// before the handler runs.
func (s *Server) guard(required Scope, h func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authorize(w, r, required, "")
		if !ok {
			return
		}
		s.audit(r.Context(), p, decisionAllowed, required, "", r)
		s.obs.Info(r.Context(), "controlplane: request",
			observe.String("principal", p.ID), observe.String("path", r.URL.Path))
		h(w, r, p)
	}
}

// guardAction is guard for an action verb, except that the handler audits the
// decision, because the grant gate runs after scope. An action so records one
// decision and never an "allowed" it then refuses.
func (s *Server) guardAction(spec ActionSpec, verb string, h func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authorize(w, r, spec.MinScope, verb)
		if !ok {
			return
		}
		h(w, r, p)
	}
}

// audit appends one access decision to the spine: principal, decision, method and
// path, verb ("" for a read), the principal's scope and the required scope. An append
// failure is logged and never fails the request. An unauthenticated attempt has the
// empty principal.
func (s *Server) audit(ctx context.Context, p Principal, decision string, required Scope, action string, r *http.Request) {
	if s.log == nil {
		return
	}
	if _, err := s.log.Append(ctx, spine.AppendInput{
		Stream:    AuditStream,
		Type:      EvAccess,
		Actor:     spine.ActorHuman,
		Principal: p.ID,
		Payload: map[string]any{
			"decision": decision,
			"method":   r.Method,
			"path":     r.URL.Path,
			"action":   action,
			"scope":    p.Scope.String(),
			"required": required.String(),
		},
	}); err != nil {
		s.obs.Error(ctx, "controlplane: audit append failed",
			observe.Err(err), observe.String("decision", decision))
	}
}

// handleList returns the live resources of a kind across all scopes.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request, _ Principal) {
	kind := r.PathValue("kind")
	rs, err := s.store.ListAll(r.Context(), kind, resource.Selector{})
	if err != nil {
		s.obs.Error(r.Context(), "controlplane: list", observe.String("kind", kind), observe.Err(err))
		writeError(w, http.StatusBadRequest, "cannot list kind "+kind)
		return
	}
	writeJSON(w, http.StatusOK, listResponse{Items: rs})
}

// handleGet returns one resource by kind and name, searching every scope as
// handleList does: global first, then the rest, so anything listable is gettable.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, _ Principal) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	res, found, err := s.resolve(r.Context(), kind, name)
	if err != nil {
		s.obs.Error(r.Context(), "controlplane: get", observe.String("kind", kind), observe.Err(err))
		writeError(w, http.StatusBadRequest, "cannot get "+kind+"/"+name)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// getAcrossScopes finds kind/name in any scope and returns the first match in the
// store's scope-then-name order, so a name in several scopes resolves
// deterministically.
func (s *Server) getAcrossScopes(ctx context.Context, kind, name string) (resource.Resource, bool, error) {
	// Use the backend's keyed lookup when it has one, instead of decoding every resource
	// of the kind; it returns the same first match.
	if g, ok := s.store.(resource.AnyScopeGetter); ok {
		return g.GetAnyScope(ctx, kind, name)
	}
	rs, err := s.store.ListAll(ctx, kind, resource.Selector{})
	if err != nil {
		return resource.Resource{}, false, err
	}
	for _, res := range rs {
		if res.Name == name {
			return res, true, nil
		}
	}
	return resource.Resource{}, false, nil
}

// handleWatch streams resource changes of a kind as server-sent events. It tails
// the resource stream from a cursor (the spine Seq), so a reconnect with
// ?after=<seq> resumes without replaying what the client already saw.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request, _ Principal) {
	kind := r.PathValue("kind")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	var cursor int64
	if a := r.URL.Query().Get("after"); a != "" {
		if n, err := strconv.ParseInt(a, 10, 64); err == nil && n > 0 {
			cursor = n
		}
	}

	// Give up after a few consecutive read errors and let the client reconnect, so a
	// broken store does not log on every poll forever.
	const maxWatchReadErrors = 5
	t := time.NewTicker(s.poll)
	defer t.Stop()
	readErrs := 0
	for {
		evs, err := s.log.Read(ctx, spine.Query{Stream: resource.ResourceStream, AfterSeq: cursor})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			readErrs++
			s.obs.Error(ctx, "controlplane: watch read", observe.Err(err), observe.Int("consecutive", readErrs))
			if readErrs >= maxWatchReadErrors {
				s.obs.Error(ctx, "controlplane: watch giving up after repeated read errors",
					observe.Int("consecutive", readErrs))
				return
			}
		} else {
			readErrs = 0
			for _, e := range evs {
				cursor = e.Seq
				res, ok := resourceEvent(e)
				if !ok || res.Kind != kind {
					continue
				}
				if err := writeSSE(w, e.Seq, res); err != nil {
					return // client went away
				}
				flusher.Flush()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// resourceEvent decodes a resource from a spine event, reporting false for any
// event that is not a resource mutation.
func resourceEvent(e spine.Event) (resource.Resource, bool) {
	switch e.Type {
	case resource.EvPut, resource.EvDeleted, resource.EvMerged:
		res, err := resource.DecodeResource(e.Payload)
		if err != nil {
			return resource.Resource{}, false
		}
		return res, true
	default:
		return resource.Resource{}, false
	}
}

// listResponse is the envelope for a list result.
type listResponse struct {
	Items []resource.Resource `json:"items"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeSSE writes one server-sent event: the spine Seq as the event id (so a
// client can resume with ?after=) and the resource JSON as the data.
func writeSSE(w http.ResponseWriter, seq int64, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, data)
	return err
}
