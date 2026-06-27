package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

func newStore(t *testing.T) (resource.Store, spine.Log) {
	t.Helper()
	reg := resource.NewRegistry()
	if err := resource.RegisterCoreKinds(reg); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(resource.Kind{
		APIVersion: "test.flynn/v1",
		Name:       "Widget",
		Schema:     json.RawMessage(`{"type":"object"}`),
		Singular:   "widget",
		Plural:     "widgets",
	}); err != nil {
		t.Fatal(err)
	}
	log := spine.NewMemoryLog()
	return resource.NewMemory(reg, resource.WithEventLog(log)), log
}

func putWidget(t *testing.T, store resource.Store, name string) {
	t.Helper()
	if _, err := store.Put(context.Background(), resource.Resource{
		APIVersion: "test.flynn/v1",
		Kind:       "Widget",
		Name:       name,
		Spec:       json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
}

func readServer(t *testing.T) *Server {
	t.Helper()
	store, log := newStore(t)
	putWidget(t, store, "w1")
	putWidget(t, store, "w2")
	auth := NewTokenAuthenticator(map[string]Principal{
		"readtok": {ID: "r", Scope: ScopeRead},
		"nonetok": {ID: "n", Scope: ScopeNone},
	})
	return NewServer(store, log, auth, WithWatchPoll(20*time.Millisecond))
}

func do(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestListAndGet(t *testing.T) {
	h := readServer(t).Handler()

	rec := do(t, h, "/v1/Widget", "readtok")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var list listResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("list items = %d, want 2", len(list.Items))
	}

	rec = do(t, h, "/v1/Widget/w1", "readtok")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	var got resource.Resource
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "w1" || got.Kind != "Widget" {
		t.Fatalf("got = %s/%s, want Widget/w1", got.Kind, got.Name)
	}

	if rec := do(t, h, "/v1/Widget/missing", "readtok"); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing status = %d, want 404", rec.Code)
	}
}

func TestAuthGate(t *testing.T) {
	h := readServer(t).Handler()

	if rec := do(t, h, "/v1/Widget", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "/v1/Widget", "bogus"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "/v1/Widget", "nonetok"); rec.Code != http.StatusForbidden {
		t.Errorf("under-scoped token = %d, want 403", rec.Code)
	}
	if rec := do(t, h, "/v1/Widget", "readtok"); rec.Code != http.StatusOK {
		t.Errorf("read token = %d, want 200", rec.Code)
	}
}

// A server built with a nil authenticator must fail closed: every request, even one
// with no token at all, is refused. Auth on by default is a construction guarantee, so
// an unauthenticated API cannot be created by omission.
func TestNilAuthFailsClosed(t *testing.T) {
	store, log := newStore(t)
	putWidget(t, store, "w1")
	h := NewServer(store, log, nil, WithWatchPoll(20*time.Millisecond)).Handler()

	for _, tok := range []string{"", "anything", "readtok"} {
		if rec := do(t, h, "/v1/Widget", tok); rec.Code != http.StatusUnauthorized {
			t.Errorf("nil-auth server with token %q = %d, want 401", tok, rec.Code)
		}
	}
}

func TestWatchStreamsNewResource(t *testing.T) {
	store, log := newStore(t)
	auth := NewTokenAuthenticator(map[string]Principal{"readtok": {ID: "r", Scope: ScopeRead}})
	srv := httptest.NewServer(NewServer(store, log, auth, WithWatchPoll(20*time.Millisecond)).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/Widget/watch", nil)
	r.Header.Set("Authorization", "Bearer readtok")
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d, want 200", resp.StatusCode)
	}

	// Create a resource after the watch is open; it must arrive on the stream.
	go func() {
		time.Sleep(50 * time.Millisecond)
		putWidget(t, store, "watched")
	}()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"watched"`) {
			return // delivered
		}
	}
	t.Fatal("watch did not deliver the new resource before the deadline")
}
