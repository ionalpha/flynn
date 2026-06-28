package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// defaultWatchPoll is how often a watch re-reads the resource stream for new
// events. The store records mutations on the spine but does not push a wake, so the
// poll is the liveness floor; it is short enough to feel live and cheap because a
// read past a cursor returns only new events.
const defaultWatchPoll = 250 * time.Millisecond

// Server is the read/watch control-plane API over a resource store. It serves
// get/list/watch for any registered kind, gated by the Authenticator, plus action
// subresources (operator/admin verbs) behind the same auth boundary and a second,
// finer gate: the caller's verified grant intersected with this instance's own local
// grant, so a narrowed remote grant can never act beyond what the target would do
// locally.
type Server struct {
	store resource.Store
	log   spine.Log // the resource stream, tailed for watch
	auth  Authenticator
	obs   observe.Logger
	poll  time.Duration

	// localGrant is this instance's own action authority: the ceiling every remote
	// caller is intersected against. It defaults to AllowAll (the zero-config
	// instance is unconstrained locally); a host narrows it to bound what any remote
	// caller can ever do here, independent of how broad a token they present.
	localGrant capability.Grant
	// admit is the capability waist the action gate runs an admitted verb through,
	// reading the effective (intersected) grant from the request context.
	admit dispatch.Admitter
	// actions maps an action verb to its spec. Verbs are registered by the
	// kill-switch and lifecycle layers via WithAction; this package owns the gate,
	// not the verbs.
	actions map[string]ActionSpec
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

// WithLocalGrant sets this instance's own action authority: the ceiling every remote
// caller's grant is intersected against, so no presented token can act beyond what
// the instance itself admits locally. The default is AllowAll (locally unconstrained).
func WithLocalGrant(g capability.Grant) Option {
	return func(s *Server) { s.localGrant = g }
}

// WithAdmitter overrides the capability waist the action gate admits a verb through.
// The default is capability.Admitter, which reads the effective grant from the
// request context; this hook exists for tests.
func WithAdmitter(a dispatch.Admitter) Option {
	return func(s *Server) {
		if a != nil {
			s.admit = a
		}
	}
}

// WithAction registers an action verb (a kube-style subresource) and its gate. The
// verb is served at POST /v1/{kind}/{name}/<verb>, refuses a token below spec.MinScope,
// and admits only if the caller's grant intersected with the instance's local grant
// permits spec.Action. Verbs with an empty Action or a nil Run are ignored, so a
// half-declared action cannot open an ungated route.
func WithAction(verb string, spec ActionSpec) Option {
	return func(s *Server) {
		if verb == "" || spec.Action == "" || spec.Run == nil {
			return
		}
		s.actions[verb] = spec
	}
}

// NewServer builds the read/watch API over store, tailing log (the store's resource
// stream) for watch, authenticated by auth. A nil auth fails closed: the server denies
// every request rather than serving openly, so an unauthenticated API cannot be created
// by omission.
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
	// Action subresources are POSTs on a named resource: POST /v1/{kind}/{name}/<verb>.
	// The literal verb segment and the POST method keep them distinct from the GET
	// read routes above. Each carries its own minimum scope and grant gate.
	for verb, spec := range s.actions {
		mux.HandleFunc("POST /v1/{kind}/{name}/"+verb, s.guardAction(spec, verb, s.handleAction(verb, spec)))
	}
	return mux
}

// Audit constants name the immutable record of who accessed the control plane. Every
// authenticated request records its decision on AuditStream as an EvAccess event, so
// the access history is a replayable fold over the spine, not a best-effort log.
const (
	// AuditStream is the spine stream every control-plane access decision is recorded
	// on, separate from the resource stream the watch tails.
	AuditStream = "controlplane.audit"
	// EvAccess is the event type of one access decision.
	EvAccess = "controlplane.access"
)

// Access decisions recorded in an audit event's payload under "decision".
const (
	decisionAllowed         = "allowed"
	decisionForbidden       = "forbidden"
	decisionUnauthenticated = "unauthenticated"
	// decisionDenied is a request that passed scope but was refused by the grant gate:
	// the caller had the scope to ask, but neither its grant nor the instance's local
	// grant admits the action. It is distinct from decisionForbidden (scope) so an
	// audit reader can tell a coarse under-scoping from a fine capability denial.
	decisionDenied = "denied"
)

// authorize authenticates a request and enforces the minimum scope, auditing and
// writing the HTTP error on failure. It returns the principal and true only when both
// pass; on a false return the response is already written and the attempt audited.
// action is the verb being attempted ("" for a read route), recorded with the decision.
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

// guard authenticates a request, enforces the minimum scope, and records the call on
// the spine before handing off. A failure is a clean 401 (unauthenticated) or 403
// (authenticated but under-scoped); each outcome, including the failures, is audited,
// because a refused or unauthenticated attempt is itself security-relevant. Reads are
// fully decided here, so a passing read is recorded allowed before the handler runs.
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

// guardAction is guard for an action subresource: it authenticates and enforces the
// verb's scope, but defers the allow/deny audit to the handler, because an action has
// a second checkpoint (the grant gate) the scope check cannot see. The handler records
// exactly one decision for the request, allowed or denied, so an action never produces
// a misleading "allowed" event it then refuses at the grant.
func (s *Server) guardAction(spec ActionSpec, verb string, h func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authorize(w, r, spec.MinScope, verb)
		if !ok {
			return
		}
		h(w, r, p)
	}
}

// audit records one access decision on the spine, the immutable audit trail by
// construction: the principal (the "who"), the decision, the request method and path,
// the action verb (empty for a read), the principal's scope, and the scope the route
// required. The spine is the system of record; a failure to append is logged but never
// fails the request, so trouble writing the audit degrades to the operability logger
// rather than taking the API down. An unauthenticated attempt carries the empty
// principal, since none was established.
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

// handleGet returns one resource by kind and name. It resolves the name across all
// scopes to match handleList, which lists every scope: the global scope is tried
// first (the unambiguous, n=1 case), then a by-name search over the other scopes,
// so a resource that is listable is also gettable rather than 404ing when it lives
// in a non-global scope.
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

// getAcrossScopes finds a resource of kind by name in any scope, used when the
// global scope has no match. It returns the first match in the store's stable order
// (scope then name), so the result is deterministic when the same name exists in
// more than one scope.
func (s *Server) getAcrossScopes(ctx context.Context, kind, name string) (resource.Resource, bool, error) {
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

	// A persistent store read error should not log on every poll forever; give up
	// after a few consecutive failures and let the client reconnect, rather than
	// turning a broken store into an endless error stream.
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
