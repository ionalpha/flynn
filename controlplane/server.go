package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
// get/list/watch for any registered kind, gated by the Authenticator. Writes and
// actions are added by later layers behind the same auth boundary.
type Server struct {
	store resource.Store
	log   spine.Log // the resource stream, tailed for watch
	auth  Authenticator
	obs   observe.Logger
	poll  time.Duration
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

// NewServer builds the read/watch API over store, tailing log (the store's resource
// stream) for watch, authenticated by auth. A nil auth fails closed: the server denies
// every request rather than serving openly, so an unauthenticated API cannot be created
// by omission.
func NewServer(store resource.Store, log spine.Log, auth Authenticator, opts ...Option) *Server {
	if auth == nil {
		auth = DenyAll{}
	}
	s := &Server{
		store: store,
		log:   log,
		auth:  auth,
		obs:   observe.Default().Log,
		poll:  defaultWatchPoll,
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
	return mux
}

// guard authenticates a request, enforces the minimum scope, and records the call
// before handing off. A failure is a clean 401 (unauthenticated) or 403
// (authenticated but under-scoped).
func (s *Server) guard(required Scope, h func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if !p.Scope.Allows(required) {
			s.obs.Info(r.Context(), "controlplane: forbidden",
				observe.String("principal", p.ID), observe.String("scope", p.Scope.String()),
				observe.String("path", r.URL.Path))
			writeError(w, http.StatusForbidden, "insufficient scope")
			return
		}
		s.obs.Info(r.Context(), "controlplane: request",
			observe.String("principal", p.ID), observe.String("path", r.URL.Path))
		h(w, r, p)
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

// handleGet returns one resource by kind and name.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, _ Principal) {
	kind, name := r.PathValue("kind"), r.PathValue("name")
	res, err := s.store.Get(r.Context(), kind, resource.Scope{}, name)
	if errors.Is(err, resource.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.obs.Error(r.Context(), "controlplane: get", observe.String("kind", kind), observe.Err(err))
		writeError(w, http.StatusBadRequest, "cannot get "+kind+"/"+name)
		return
	}
	writeJSON(w, http.StatusOK, res)
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

	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		evs, err := s.log.Read(ctx, spine.Query{Stream: resource.ResourceStream, AfterSeq: cursor})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.obs.Error(ctx, "controlplane: watch read", observe.Err(err))
		}
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
