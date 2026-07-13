package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/testkit"
	"github.com/ionalpha/flynn/observe"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/spine"
)

// widgetDescriptor is the read-model view of the test kind: one column, so a projection
// failure is unambiguous.
func widgetDescriptor() Descriptor {
	return Descriptor{
		Kind:    "Widget",
		Columns: []Column{{Header: "NAME", Project: Name()}},
	}
}

// missingGetStore reports every named lookup as absent but fails the listing behind it,
// so the by-name fallback path is exercised against a broken backend. It also hides the
// keyed any-scope lookup, which is the shape of a store that has no name index.
type missingGetStore struct {
	resource.Store
	listErr error
}

func (m missingGetStore) Get(context.Context, string, resource.Scope, string) (resource.Resource, error) {
	return resource.Resource{}, resource.ErrNotFound
}

func (m missingGetStore) ListAll(ctx context.Context, kind string, sel resource.Selector) ([]resource.Resource, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.Store.ListAll(ctx, kind, sel)
}

// TestReadModelSurfacesStoreFailures checks List and Describe report a broken backend
// rather than returning an empty table that reads as "nothing exists".
func TestReadModelSurfacesStoreFailures(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	broken := errors.New("backing store unavailable")
	faulty := faultyStore{Store: store, err: broken}

	if _, err := List(context.Background(), faulty, widgetDescriptor(), nil); !errors.Is(err, broken) {
		t.Errorf("List error = %v, want %v", err, broken)
	}
	if _, err := Describe(context.Background(), faulty, log, widgetDescriptor(), "any-id", 0); !errors.Is(err, broken) {
		t.Errorf("Describe error = %v, want %v", err, broken)
	}
}

// TestDescribeSurfacesLogFailure checks a resource that reads fine but whose history
// cannot be read is an error, not a resource silently described with no history.
func TestDescribeSurfacesLogFailure(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	r := getWidget(t, store, "w1")

	broken := errors.New("spine unavailable")
	_, err := Describe(context.Background(), store, readFailingLog{Log: log, err: broken}, widgetDescriptor(), r.ID, 0)
	if !errors.Is(err, broken) {
		t.Fatalf("Describe error = %v, want %v", err, broken)
	}
}

// TestDescribeHistoryIsFilteredAndTailed checks the history read: events that are not
// this resource's mutations are left out, an event whose payload is not a resource is
// skipped rather than failing the describe, and a positive tail keeps only the most
// recent events.
func TestDescribeHistoryIsFilteredAndTailed(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	putWidget(t, store, "w2") // another resource's events must not appear
	r := getWidget(t, store, "w1")

	// Three mutations of w1 in total: the create above, plus two updates.
	for range 2 {
		cur := getWidget(t, store, "w1")
		cur.Spec = json.RawMessage(`{"n":1}`)
		if _, err := store.Put(context.Background(), cur); err != nil {
			t.Fatalf("update widget: %v", err)
		}
	}
	// An event on the resource stream whose payload is not a resource at all. It must be
	// skipped, not fail the describe: the stream is shared and a reader tolerates what it
	// cannot decode.
	if _, err := log.Append(context.Background(), spine.AppendInput{
		Stream:  resource.ResourceStream,
		Type:    resource.EvPut,
		Actor:   spine.ActorSystem,
		Payload: map[string]any{"resource": "not a resource"},
	}); err != nil {
		t.Fatalf("append junk event: %v", err)
	}

	all, err := Describe(context.Background(), store, log, widgetDescriptor(), r.ID, 0)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(all.Events) != 3 {
		t.Fatalf("history = %d events, want the 3 mutations of this resource", len(all.Events))
	}
	if all.Row.Name != "w1" {
		t.Errorf("row name = %q, want w1", all.Row.Name)
	}

	tailed, err := Describe(context.Background(), store, log, widgetDescriptor(), r.ID, 1)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tailed.Events) != 1 {
		t.Fatalf("tailed history = %d events, want 1", len(tailed.Events))
	}
	if tailed.Events[0].Seq != all.Events[2].Seq {
		t.Errorf("tail kept seq %d, want the most recent %d", tailed.Events[0].Seq, all.Events[2].Seq)
	}
}

// TestDescribeWithoutALogHasNoHistory checks a nil log is a resource with no history
// rather than an error: history is optional, the resource is not.
func TestDescribeWithoutALogHasNoHistory(t *testing.T) {
	store, _ := newStore(t)
	putWidget(t, store, "w1")
	r := getWidget(t, store, "w1")

	d, err := Describe(context.Background(), store, nil, widgetDescriptor(), r.ID, 0)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(d.Events) != 0 {
		t.Errorf("history = %d events, want none without a log", len(d.Events))
	}
}

// getWidget reads back a widget by name.
func getWidget(t *testing.T, store resource.Store, name string) resource.Resource {
	t.Helper()
	r, err := store.Get(context.Background(), "Widget", resource.Scope{}, name)
	if err != nil {
		t.Fatalf("get widget %s: %v", name, err)
	}
	return r
}

// TestFieldProjectionRendersEveryScalarKind checks the cell renderer: each JSON scalar
// prints in its canonical form, an integral number carries no fraction, and a composite
// falls back to compact JSON.
func TestFieldProjectionRendersEveryScalarKind(t *testing.T) {
	r := resource.Resource{
		Spec: json.RawMessage(`{
          "s": "text", "b": true, "i": 7, "f": 1.5, "z": null,
          "arr": [1, 2], "obj": {"k": "v"}
        }`),
	}
	cases := []struct {
		path []string
		want string
	}{
		{[]string{"s"}, "text"},
		{[]string{"b"}, "true"},
		{[]string{"i"}, "7"},
		{[]string{"f"}, "1.5"},
		{[]string{"z"}, ""},
		{[]string{"arr"}, "[1,2]"},
		{[]string{"obj"}, `{"k":"v"}`},
		{[]string{"obj", "k"}, "v"},
		{[]string{"missing"}, ""},
		{[]string{"s", "deeper"}, ""}, // a path through a scalar has no value
		{nil, ""},                     // no path at all
	}
	for _, tc := range cases {
		if got := SpecField(tc.path...)(r); got != tc.want {
			t.Errorf("SpecField(%v) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestFieldProjectionOfUnreadableJSON checks a resource whose stored JSON cannot be
// parsed projects to an empty cell rather than panicking or failing the whole table.
func TestFieldProjectionOfUnreadableJSON(t *testing.T) {
	r := resource.Resource{
		Spec:   json.RawMessage(`{not json`),
		Status: json.RawMessage(``),
	}
	if got := SpecField("color")(r); got != "" {
		t.Errorf("SpecField over unreadable JSON = %q, want an empty cell", got)
	}
	if got := StatusField("state")(r); got != "" {
		t.Errorf("StatusField over an absent status = %q, want an empty cell", got)
	}
	// Diff walks the same JSON: an unreadable or absent side contributes no fields
	// rather than failing.
	if d := Diff(r, r); len(d) != 0 {
		t.Errorf("Diff of unreadable JSON = %v, want no deltas", d)
	}
}

// TestPollWatcherSurfacesStoreFailure checks a broken store is reported rather than
// looking like every resource was deleted, which is what an empty list would mean.
func TestPollWatcherSurfacesStoreFailure(t *testing.T) {
	store, _ := newStore(t)
	broken := errors.New("backing store unavailable")
	w := NewPollWatcher(faultyStore{Store: store, err: broken}, "Widget", resource.Selector{})

	if _, err := w.Poll(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("Poll error = %v, want %v", err, broken)
	}
}

// TestPollWatcherOrdersSameNameByID checks the change order is total: two resources of
// the same name in different scopes are ordered by id, so a poll reports the same
// sequence every time.
func TestPollWatcherOrdersSameNameByID(t *testing.T) {
	store, _ := newStore(t)
	for _, scope := range []resource.Scope{{}, {Project: "p"}} {
		if _, err := store.Put(context.Background(), resource.Resource{
			APIVersion: "test.flynn/v1",
			Kind:       "Widget",
			Name:       "same",
			Scope:      scope,
			Spec:       json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("put widget in scope %v: %v", scope, err)
		}
	}

	w := NewPollWatcher(store, "Widget", resource.Selector{})
	changes, err := w.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %d, want 2 additions", len(changes))
	}
	if changes[0].Resource.ID >= changes[1].Resource.ID {
		t.Errorf("changes with the same name are not ordered by id: %q then %q",
			changes[0].Resource.ID, changes[1].Resource.ID)
	}
}

// TestServerOptionsIgnoreEmptyValues checks the options refuse to install nothing: a nil
// logger or admitter leaves the safe default in place, and a half-declared action opens
// no route at all.
func TestServerOptionsIgnoreEmptyValues(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")

	srv := NewServer(store, log, readAuth(),
		WithLogger(nil),
		WithAdmitter(nil),
		WithWatchPoll(0),
		WithAction("", ActionSpec{Action: "a", MinScope: ScopeOperator, Run: nilRun}),
		WithAction("noaction", ActionSpec{MinScope: ScopeOperator, Run: nilRun}),
		WithAction("norun", ActionSpec{Action: "a", MinScope: ScopeOperator}),
	)
	if srv.admit == nil {
		t.Error("a nil admitter must leave the default in place")
	}
	if srv.poll != defaultWatchPoll {
		t.Errorf("poll = %v, want the default %v", srv.poll, defaultWatchPoll)
	}
	if len(srv.actions) != 0 {
		t.Fatalf("actions = %v, want a half-declared verb to be ignored", srv.actions)
	}

	// A route that was never registered answers 404, so a half-declared verb cannot be
	// reached at all.
	h := srv.Handler()
	for _, verb := range []string{"noaction", "norun"} {
		rec := doPost(t, h, "/v1/Widget/w1/"+verb, "readtok")
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s status = %d, want 404", verb, rec.Code)
		}
	}
}

func nilRun(context.Context, Principal, resource.Resource) (any, error) { return nil, nil }

// TestServerOptionsInstallOverrides checks the same options do install a real value, so
// the guards above are not simply ignoring everything.
func TestServerOptionsInstallOverrides(t *testing.T) {
	store, log := newStore(t)
	srv := NewServer(store, log, readAuth(),
		WithLogger(observe.Default().Log),
		WithAdmitter(allowAdmitter{}),
		WithWatchPoll(5*time.Millisecond),
	)
	if _, ok := srv.admit.(allowAdmitter); !ok {
		t.Errorf("admitter = %T, want the injected one", srv.admit)
	}
	if srv.poll != 5*time.Millisecond {
		t.Errorf("poll = %v, want the override", srv.poll)
	}
}

// allowAdmitter admits every action, standing in for the capability waist.
type allowAdmitter struct{}

func (allowAdmitter) Admit(context.Context, dispatch.Action) error { return nil }

// TestAFailingAuditAppendDoesNotFailTheRequest checks the audit trail degrading does not
// take the API down: the read still answers, and the failure lands on the logger.
func TestAFailingAuditAppendDoesNotFailTheRequest(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	broken := testkit.FaultyLog(log, testkit.Always(errors.New("spine unavailable")))

	h := NewServer(store, broken, readAuth()).Handler()
	rec := do(t, h, "/v1/Widget", "readtok")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 despite a failing audit append", rec.Code)
	}
}

// TestGetFallsBackToAScanWhenTheStoreHasNoNameIndex checks the by-name resolution path a
// store without a keyed any-scope lookup takes: the kind is listed and scanned, so a
// resource that is listable stays gettable.
func TestGetFallsBackToAScanWhenTheStoreHasNoNameIndex(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")

	h := NewServer(missingGetStore{Store: store}, log, readAuth()).Handler()

	rec := do(t, h, "/v1/Widget/w1", "readtok")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 from the scan fallback", rec.Code)
	}
	rec = do(t, h, "/v1/Widget/absent", "readtok")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get of an absent name = %d, want 404", rec.Code)
	}
}

// TestGetReportsAFailingScan checks the fallback scan failing is an error status rather
// than a 404: "the store is broken" is not "the resource does not exist".
func TestGetReportsAFailingScan(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	broken := missingGetStore{Store: store, listErr: errors.New("backing store unavailable")}

	h := NewServer(broken, log, readAuth()).Handler()
	rec := do(t, h, "/v1/Widget/w1", "readtok")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("get status = %d, want 400 when the scan fails", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot get") {
		t.Errorf("body = %q, want it to report the failure", rec.Body.String())
	}
}

// unflushableWriter is an http.ResponseWriter that cannot stream, which is what a
// wrapping middleware without a Flush method leaves the watch handler holding.
type unflushableWriter struct{ rec *httptest.ResponseRecorder }

func (u unflushableWriter) Header() http.Header         { return u.rec.Header() }
func (u unflushableWriter) Write(p []byte) (int, error) { return u.rec.Write(p) }
func (u unflushableWriter) WriteHeader(code int)        { u.rec.WriteHeader(code) }

// TestWatchRefusesAWriterThatCannotStream checks the watch refuses rather than buffering
// a stream nobody will ever receive.
func TestWatchRefusesAWriterThatCannotStream(t *testing.T) {
	h := readServer(t).Handler()
	rec := httptest.NewRecorder()
	w := unflushableWriter{rec: rec}

	h.ServeHTTP(w, watchRequest(t))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("watch status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "streaming unsupported") {
		t.Errorf("body = %q, want it to name the reason", rec.Body.String())
	}
}

// TestWatchAcceptsAnyCursorValue checks the ?after= cursor is read where it is usable
// and ignored where it is not: a garbled or negative resume degrades to a full stream
// rather than refusing the client. The request context is already cancelled, so the
// handler makes exactly one pass and returns.
func TestWatchAcceptsAnyCursorValue(t *testing.T) {
	srv := readServer(t)
	h := srv.Handler()

	for _, query := range []string{"?after=1", "?after=notanumber", "?after=-4", ""} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client is already gone: the handler must make one pass and return
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/Widget/watch"+query, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		r.Header.Set("Authorization", "Bearer readtok")
		rec := httptest.NewRecorder()

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(rec, r)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("watch%s did not return for a cancelled client", query)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("watch%s status = %d, want 200", query, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("watch%s content type = %q, want an event stream", query, got)
		}
	}
}

// TestResourceEventIgnoresWhatIsNotAResourceMutation checks the watch's event filter:
// an event of another type, and a mutation whose payload will not decode, are both
// skipped rather than streamed as an empty resource.
func TestResourceEventIgnoresWhatIsNotAResourceMutation(t *testing.T) {
	if _, ok := resourceEvent(spine.Event{Type: "some.other.event"}); ok {
		t.Error("an unrelated event type must not decode as a resource mutation")
	}
	if _, ok := resourceEvent(spine.Event{
		Type:    resource.EvPut,
		Payload: map[string]any{"resource": "not a resource"},
	}); ok {
		t.Error("a mutation whose payload is not a resource must not decode")
	}
	res, ok := resourceEvent(spine.Event{
		Type:    resource.EvDeleted,
		Payload: map[string]any{"resource": map[string]any{"kind": "Widget", "name": "w1"}},
	})
	if !ok || res.Name != "w1" {
		t.Errorf("resourceEvent = %+v, %v, want the decoded resource", res, ok)
	}
}

// TestWriteSSERefusesUnserializableData checks the stream writer reports a value it
// cannot render rather than emitting a half-written event.
func TestWriteSSERefusesUnserializableData(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeSSE(rec, 1, make(chan int)); err == nil {
		t.Fatal("writeSSE must refuse a value it cannot marshal")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a refused event still wrote %q", rec.Body.String())
	}
}

// TestGeneratedOperatorRefusesAnEmptyOrFailingMint checks the auth-on-by-default path
// fails closed: a mint that errors, or one that hands back an empty token, yields no
// authenticator rather than a server anyone can call.
func TestGeneratedOperatorRefusesAnEmptyOrFailingMint(t *testing.T) {
	broken := errors.New("entropy source unavailable")
	if _, _, err := GeneratedOperator("op", ScopeOperator, func() (string, error) {
		return "", broken
	}); !errors.Is(err, broken) {
		t.Errorf("GeneratedOperator error = %v, want %v", err, broken)
	}
	if _, _, err := GeneratedOperator("op", ScopeOperator, func() (string, error) {
		return "", nil
	}); err == nil {
		t.Error("an empty generated token must be refused")
	}
}

// TestLoadOrCreateIdentityReportsAFailedSeal checks first-run persistence failing is an
// error rather than an identity that vanishes on the next restart.
func TestLoadOrCreateIdentityReportsAFailedSeal(t *testing.T) {
	_, err := LoadOrCreateIdentity(context.Background(), sealFailingVault{}, "")
	if err == nil || !strings.Contains(err.Error(), "persist identity") {
		t.Fatalf("LoadOrCreateIdentity error = %v, want a persist failure", err)
	}
}

// sealFailingVault has no seed and cannot store one, which is the first-run case against
// a vault that is readable but not writable.
type sealFailingVault struct{}

func (sealFailingVault) Lookup(context.Context, string) (secret.Text, error) {
	return secret.Text{}, secret.ErrNotFound
}

func (sealFailingVault) Set(context.Context, string, secret.Text) error {
	return errors.New("vault is read-only")
}

// TestActionReportsAFailingResolve checks a verb whose target cannot be looked up is a
// clean error rather than an action on an unknown resource.
func TestActionReportsAFailingResolve(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	broken := faultyStore{Store: store, err: errors.New("backing store unavailable")}

	var ran bool
	srv := NewServer(broken, log, actionAuth(), WithLocalGrant(capability.AllowAll()),
		WithAction("halt", ActionSpec{
			Action:   haltActionName,
			MinScope: ScopeOperator,
			Run: func(context.Context, Principal, resource.Resource) (any, error) {
				ran = true
				return nil, nil
			},
		}))

	rec := doPost(t, srv.Handler(), "/v1/Widget/w1/halt", "optok")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("action status = %d, want 400 when the target cannot be resolved", rec.Code)
	}
	if ran {
		t.Error("the verb ran against a resource that could not be resolved")
	}
}

// TestActionMapsARunFailureToAStatus checks the verb's own failure reaches the caller
// with the right status: a refusal is a 403, anything else a 500, and neither is
// reported as success.
func TestActionMapsARunFailureToAStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"refused by the verb", fault.Wrap(fault.Forbidden, "not_permitted", errors.New("not permitted here")), http.StatusForbidden},
		{"failed inside the verb", errors.New("disk on fire"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, log := newStore(t)
			putWidget(t, store, "w1")
			srv := NewServer(store, log, actionAuth(), WithLocalGrant(capability.AllowAll()),
				WithAction("halt", ActionSpec{
					Action:   haltActionName,
					MinScope: ScopeOperator,
					Run: func(context.Context, Principal, resource.Resource) (any, error) {
						return nil, tc.err
					},
				}))

			rec := doPost(t, srv.Handler(), "/v1/Widget/w1/halt", "optok")
			if rec.Code != tc.status {
				t.Fatalf("action status = %d, want %d", rec.Code, tc.status)
			}
			if !strings.Contains(rec.Body.String(), "failed") {
				t.Errorf("body = %q, want it to report the failure", rec.Body.String())
			}
			// The decision was still recorded as allowed: the gate admitted it, the verb
			// itself failed. Those are different facts and the audit keeps them apart.
			if got := decisionOf(t, lastAudit(t, log)); got != decisionAllowed {
				t.Errorf("audited decision = %q, want %q", got, decisionAllowed)
			}
		})
	}
}

// actionAuth is the token table the action tests authenticate against: one operator with
// full authority.
func actionAuth() Authenticator {
	return NewTokenAuthenticator(map[string]Principal{
		"optok":   {ID: "op", Scope: ScopeOperator, Grant: capability.AllowAll()},
		"readtok": {ID: "r", Scope: ScopeRead, Grant: capability.AllowAll()},
	})
}

// decisionOf reads the decision out of an audit event's payload.
func decisionOf(t *testing.T, ev spine.Event) string {
	t.Helper()
	decision, ok := ev.Payload["decision"].(string)
	if !ok {
		t.Fatalf("audit event carries no decision: %v", ev.Payload)
	}
	return decision
}
