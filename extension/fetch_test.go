package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// fetchStub is a stub extension tool that speaks the fetch half of the host-call handshake: on
// its first call it opens a session and asks the host to send a request body; on each
// continuation it records the response it was given and asks for the next one, until it has made
// `rounds` of them and returns a terminal result. It lets a test prove the host drives the loop
// and hands back exactly the bytes the endpoint returned.
type fetchStub struct {
	rounds int

	mu        sync.Mutex
	bodies    [][]byte // the request bodies it asked the host to send, in order
	responses [][]byte // the response bodies it received, in order
	fetchErrs []string // any fetch-failure messages it received
	seq       int
	done      map[string]int
}

func newFetchStub(rounds int) *fetchStub { return &fetchStub{rounds: rounds, done: map[string]int{}} }

func (s *fetchStub) Def() llm.Tool {
	return llm.Tool{Name: "fetch", Description: "host-sent op", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (s *fetchStub) Invoke(_ context.Context, in json.RawMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cont struct {
		Session  string `json:"session"`
		Response string `json:"response"`
		FetchErr string `json:"fetchError"`
	}
	_ = json.Unmarshal(in, &cont)

	if cont.Session == "" {
		s.seq++
		return s.emit(fmt.Sprintf("f%d", s.seq))
	}
	if cont.FetchErr != "" {
		s.fetchErrs = append(s.fetchErrs, cont.FetchErr)
		return `{"done":true,"error":"` + cont.FetchErr + `"}`, nil
	}
	res, err := base64.StdEncoding.DecodeString(cont.Response)
	if err != nil {
		return "", err
	}
	s.responses = append(s.responses, res)
	s.done[cont.Session]++
	return s.emit(cont.Session)
}

// emit returns the next fetch request for a session, or the terminal result once the session has
// made all its round-trips.
func (s *fetchStub) emit(id string) (string, error) {
	if s.done[id] >= s.rounds {
		return `{"done":true,"result":"ok"}`, nil
	}
	body := fmt.Appendf(nil, "request-%s-%d", id, s.done[id])
	s.bodies = append(s.bodies, body)
	return `{"session":"` + id + `","fetch":{"body":"` + base64.StdEncoding.EncodeToString(body) + `"}}`, nil
}

// recordingFetcher is a HostFetcher that answers from memory, recording what it was asked to send.
// It stands in for the endpoint the operator granted: note that the tool never names it.
type recordingFetcher struct {
	mu   sync.Mutex
	sent [][]byte
	err  error
}

func (f *recordingFetcher) Fetch(_ context.Context, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.sent = append(f.sent, append([]byte(nil), body...))
	return append([]byte("response-to:"), body...), nil
}

// TestHostFetchDrivesHandshake proves the host runs the full fetch loop for a network-borrowing
// tool: it sends each request body the tool hands out through the GRANTED fetcher, feeds the
// response back, and returns the terminal result. The tool never named a destination, so the
// only place its bytes could go is the endpoint the host holds.
func TestHostFetchDrivesHandshake(t *testing.T) {
	stub := newFetchStub(3)
	fetcher := &recordingFetcher{}
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostFetcher(func(ext, tool string) HostFetcher {
			if ext == "token" && tool == "fetch" {
				return fetcher
			}
			return nil
		}))

	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{"foo":"bar"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, `"done":true`) || !strings.Contains(out, `"ok"`) {
		t.Fatalf("did not get the terminal result: %q", out)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	fetcher.mu.Lock()
	defer fetcher.mu.Unlock()

	if len(stub.bodies) != 3 || len(stub.responses) != 3 || len(fetcher.sent) != 3 {
		t.Fatalf("want 3 round-trips, got %d asked / %d sent / %d answered",
			len(stub.bodies), len(fetcher.sent), len(stub.responses))
	}
	for i, body := range stub.bodies {
		if string(fetcher.sent[i]) != string(body) {
			t.Fatalf("round-trip %d: host sent %q, the tool asked to send %q", i, fetcher.sent[i], body)
		}
		want := "response-to:" + string(body)
		if string(stub.responses[i]) != want {
			t.Fatalf("round-trip %d: tool got %q, want the endpoint's answer %q", i, stub.responses[i], want)
		}
	}
}

// TestHostFetchWithoutGrantIsRefused proves a tool that asks the host to send something without
// having been granted an endpoint is refused. Default-deny: mounting a tool grants it nothing.
func TestHostFetchWithoutGrantIsRefused(t *testing.T) {
	stub := newFetchStub(1)
	// A signer is granted (so the host-call loop runs at all) but no fetcher is.
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostSigner(func(string, string) HostSigner { return testSigner(t) }))

	_, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a tool with no granted endpoint must not be able to make the host send anything")
	}
	if !strings.Contains(err.Error(), "granted no endpoint") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUngrantedHostCallIsNotLeakedAsAResult pins a hole worth naming: a tool granted NOTHING at
// all still has its host-call messages refused, rather than passed back to the caller as if they
// were a result. Handing a "fetch" message to the model as output would be both a broken result
// and a channel for an extension to ask the model to make the call on its behalf. Default-deny
// means the request is stopped here.
func TestUngrantedHostCallIsNotLeakedAsAResult(t *testing.T) {
	stub := newFetchStub(1)
	h, _, m := mountStub(t, []mission.Tool{stub}) // no signer, no fetcher: no authority whatsoever

	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("an ungranted host call must be refused, got result: %q", out)
	}
	if strings.Contains(out, "fetch") {
		t.Fatalf("the tool's host-call message was leaked to the caller: %q", out)
	}
}

// TestHostFetchDeliversFailure proves a fetch error is delivered to the tool as a fetch-failure
// message (so the tool runs its own failure path, unwinding whatever it already did) rather than
// aborting the call from under it.
func TestHostFetchDeliversFailure(t *testing.T) {
	stub := newFetchStub(3)
	fetcher := &recordingFetcher{err: errors.New("endpoint unreachable")}
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostFetcher(func(string, string) HostFetcher { return fetcher }))

	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.fetchErrs) != 1 || !strings.Contains(stub.fetchErrs[0], "endpoint unreachable") {
		t.Fatalf("fetch failure was not delivered to the tool: %v", stub.fetchErrs)
	}
	if !strings.Contains(out, `"done":true`) {
		t.Fatalf("tool did not reach a terminal result after the fetch failure: %q", out)
	}
}

// TestHostFetchBudgetEnforced proves a tool that never terminates the handshake is stopped at the
// fetch limit rather than pumping the host's network in a loop forever.
func TestHostFetchBudgetEnforced(t *testing.T) {
	stub := newFetchStub(1000) // never terminates within the budget
	h, _, m := mountStub(t, []mission.Tool{stub},
		WithHostFetcher(func(string, string) HostFetcher { return &recordingFetcher{} }),
		WithMaxFetches(3))

	if _, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the fetch budget to stop an unbounded fetch loop")
	}
}

// TestHTTPHostFetcherRefusesPrivateEndpointByDefault proves the anti-SSRF default: a grant cannot
// silently aim the host's network at loopback, a private range, or the cloud metadata endpoint.
// A deliberate local endpoint is possible, but only with the explicit opt-in.
func TestHTTPHostFetcherRefusesPrivateEndpointByDefault(t *testing.T) {
	private := []string{
		"http://127.0.0.1:8899",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5:8899",
		"http://[::1]:8899",
	}
	for _, endpoint := range private {
		if _, err := NewHTTPHostFetcher(endpoint); err == nil {
			t.Errorf("a private endpoint must be refused without the explicit opt-in: %s", endpoint)
		}
		if _, err := NewHTTPHostFetcher(endpoint, WithPrivateEndpoint()); err != nil {
			t.Errorf("the explicit opt-in must permit a literal private endpoint (%s): %v", endpoint, err)
		}
	}
	// A NAME is refused even with the opt-in: resolving it here could differ from the address
	// actually dialled, which is the rebinding hole the dial-time check exists to close.
	if _, err := NewHTTPHostFetcher("http://localhost:8899", WithPrivateEndpoint()); err == nil {
		t.Error("a private endpoint given as a name must be refused; only a literal IP is honoured")
	}
	// A non-HTTP scheme is not an endpoint at all.
	if _, err := NewHTTPHostFetcher("file:///etc/passwd"); err == nil {
		t.Error("a non-http scheme must be refused")
	}
}

// TestHTTPHostFetcherSendsAndBounds proves the fetcher POSTs the body to the endpoint IT holds,
// returns the response, and refuses an over-sized one rather than truncating it into a half-parsed
// answer or exhausting host memory.
func TestHTTPHostFetcherSendsAndBounds(t *testing.T) {
	var got []byte
	var gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = readAll(r)
		gotType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"result":"pong"}`))
	}))
	defer srv.Close()

	// The test server listens on loopback, which is exactly the deliberate-local-endpoint case.
	f, err := NewHTTPHostFetcher(srv.URL, WithPrivateEndpoint())
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	res, err := f.Fetch(context.Background(), []byte(`{"method":"ping"}`))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(res) != `{"result":"pong"}` {
		t.Fatalf("response body not returned verbatim: %q", res)
	}
	if string(got) != `{"method":"ping"}` {
		t.Fatalf("request body not sent verbatim: %q", got)
	}
	if gotType != "application/json" {
		t.Fatalf("content type = %q, want application/json", gotType)
	}

	// The same endpoint, with the response cap set below the answer's size, is refused.
	small, err := NewHTTPHostFetcher(srv.URL, WithPrivateEndpoint(), WithMaxResponseBytes(4))
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	if _, err := small.Fetch(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("an over-sized response must be refused, not truncated")
	}
}

// TestHTTPHostFetcherReportsBadStatus proves a non-2xx answer is an error carrying the status, and
// that the endpoint's body is not passed through: an error page must not become a channel into the
// extension.
func TestHTTPHostFetcherReportsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("secret-error-page"))
	}))
	defer srv.Close()

	f, err := NewHTTPHostFetcher(srv.URL, WithPrivateEndpoint())
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	body, err := f.Fetch(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatal("a non-2xx status must be an error")
	}
	if !strings.Contains(err.Error(), "418") {
		t.Fatalf("the error should carry the status: %v", err)
	}
	if strings.Contains(string(body), "secret-error-page") {
		t.Fatal("the endpoint's error body must not be handed to the extension")
	}
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
