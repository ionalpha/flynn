package e2e

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSignalDaemon speaks the slice of signal-cli's JSON-RPC protocol that serve uses: it
// pushes one "receive" notification when the client connects, and answers each "send" the
// client makes, keeping what was sent. A real daemon needs a registered phone number, which
// a test cannot have; the protocol is the contract serve is tested against.
type fakeSignalDaemon struct {
	addr  string
	sent  chan string
	mu    sync.Mutex
	conns []net.Conn
}

func newFakeSignalDaemon(t *testing.T, sender, text string) *fakeSignalDaemon {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeSignalDaemon{addr: ln.Addr().String(), sent: make(chan string, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.conns = append(d.conns, c)
			d.mu.Unlock()
			go d.serve(c, sender, text)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, c := range d.conns {
			_ = c.Close()
		}
	})
	return d
}

func (d *fakeSignalDaemon) serve(c net.Conn, sender, text string) {
	note, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "receive",
		"params": map[string]any{"envelope": map[string]any{
			"source":      sender,
			"dataMessage": map[string]any{"message": text},
		}},
	})
	_, _ = c.Write(append(note, '\n'))

	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var req struct {
			ID     *uint64 `json:"id"`
			Method string  `json:"method"`
			Params struct {
				Message string `json:"message"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		if req.Method == "send" {
			select {
			case d.sent <- req.Params.Message:
			default:
			}
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": map[string]any{}})
		_, _ = c.Write(append(resp, '\n'))
	}
}

// freeAddr returns a loopback address nothing is listening on.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

// apiGet requests path from the control-plane API, with the bearer token when it is not
// empty, and returns the status and body.
func apiGet(t *testing.T, addr, path, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestLiveServeAnswersAChannelMessageAndExposesTheRun starts the daemon against a fake
// Signal daemon and the read-only API. A message from an allowed number starts a run under
// the spend caps; the model's answer comes back over the channel; the API, which refuses a
// request without the bearer token, lists the goal that run created.
func TestLiveServeAnswersAChannelMessageAndExposesTheRun(t *testing.T) {
	in := liveInstance(t, liveModel(t))
	const sender = "+15550001234"
	daemon := newFakeSignalDaemon(t, sender, "Reply with one short sentence saying you got this message.")
	api := freeAddr(t)
	const token = "live-e2e-token"

	serve := in.start(append(append(liveCaps(), "-no-learn"),
		"serve", "--api-addr", api, "--api-token", token,
		"--signal-tcp", daemon.addr, "--signal-allow", sender)...)

	select {
	case reply := <-daemon.sent:
		if strings.TrimSpace(reply) == "" {
			t.Error("serve sent an empty reply over the channel")
		}
	case err := <-serve.done:
		t.Fatalf("serve exited before answering: %v\nstderr:\n%s", err, serve.errb.String())
	case <-time.After(defaultLiveTimeout):
		t.Fatalf("no reply over the channel within %s\nstderr:\n%s", defaultLiveTimeout, serve.errb.String())
	}

	if code, _ := apiGet(t, api, "/v1/Goal", ""); code != http.StatusUnauthorized {
		t.Errorf("an API request without the token got status %d, want 401", code)
	}
	code, body := apiGet(t, api, "/v1/Goal", token)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/Goal with the token got status %d: %s", code, body)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode the goal list: %v\n%s", err, body)
	}
	if len(list.Items) == 0 {
		t.Errorf("the API lists no goal although the channel message started one: %s", body)
	}
	if code, _ := apiGet(t, api, "/v1/Goal", "wrong-token"); code != http.StatusUnauthorized {
		t.Errorf("an API request with a wrong token got status %d, want 401", code)
	}
}

// mcpSession is a running `flynn mcp serve` driven over its stdio.
type mcpSession struct {
	t      *testing.T
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

// startMCP launches the MCP server in the instance's workspace, completes the handshake
// and returns the session. The server needs no model.
func startMCP(t *testing.T, in *instance, flags ...string) *mcpSession {
	t.Helper()
	args := append([]string{"-data-dir", in.dataDir, "mcp", "serve", "--workdir", in.workspace}, flags...)
	cmd := exec.Command(flynnBin, args...)
	cmd.Dir = in.workspace
	cmd.Env = in.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mcp serve: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	s := &mcpSession{t: t, stdin: stdin, stdout: bufio.NewReader(stdout)}
	s.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "flynn-e2e", "version": "0"},
	})
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *mcpSession) send(msg map[string]any) {
	s.t.Helper()
	b, _ := json.Marshal(msg)
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write to mcp serve: %v", err)
	}
}

// call sends one request and returns its result, failing on a protocol error or when no
// response arrives in time.
func (s *mcpSession) call(method string, params any) map[string]any {
	s.t.Helper()
	s.nextID++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params})

	type frame struct {
		line string
		err  error
	}
	got := make(chan frame, 1)
	go func() {
		line, err := s.stdout.ReadString('\n')
		got <- frame{line, err}
	}()
	select {
	case f := <-got:
		if f.err != nil {
			s.t.Fatalf("read from mcp serve: %v", f.err)
		}
		var resp struct {
			Result map[string]any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(f.line), &resp); err != nil {
			s.t.Fatalf("mcp serve sent a non-JSON line: %q", f.line)
		}
		if resp.Error != nil {
			s.t.Fatalf("%s: protocol error: %s", method, resp.Error.Message)
		}
		return resp.Result
	case <-time.After(60 * time.Second):
		s.t.Fatalf("%s: no response from mcp serve", method)
		return nil
	}
}

// tools returns the names the server lists.
func (s *mcpSession) tools() []string {
	s.t.Helper()
	var names []string
	list, _ := s.call("tools/list", map[string]any{})["tools"].([]any)
	for _, v := range list {
		if m, ok := v.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	return names
}

// invoke calls a tool and reports its text and whether the server marked it an error.
func (s *mcpSession) invoke(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	res := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	var text []string
	content, _ := res["content"].([]any)
	for _, v := range content {
		if m, ok := v.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				text = append(text, s)
			}
		}
	}
	isErr, _ := res["isError"].(bool)
	return strings.Join(text, "\n"), isErr
}

// TestLiveMCPServesTheToolsAndReadOnlyDeniesTheRest drives the shipped binary as an MCP
// server over stdio. The full server writes a file and reads it back; the read-only server
// refuses a write and a shell command when they are called (it still lists
// them), leaving the disk as it was. The server needs no model, so this runs wherever the
// lane does.
func TestLiveMCPServesTheToolsAndReadOnlyDeniesTheRest(t *testing.T) {
	requireLive(t)

	t.Run("full server writes and reads", func(t *testing.T) {
		in := newInstance(t)
		in.model = ""
		s := startMCP(t, in)
		if names := s.tools(); !containsString(names, "write") || !containsString(names, "read") {
			t.Fatalf("tools/list = %v, want at least write and read", names)
		}
		if out, isErr := s.invoke("write", map[string]any{"path": "viamcp.txt", "content": "from mcp\n"}); isErr {
			t.Fatalf("write through MCP failed: %s", out)
		}
		got, err := in.workfile("viamcp.txt")
		if err != nil || string(got) != "from mcp\n" {
			t.Errorf("the file written through MCP is %q (%v)", got, err)
		}
		out, isErr := s.invoke("read", map[string]any{"path": "viamcp.txt"})
		if isErr || !strings.Contains(out, "from mcp") {
			t.Errorf("read through MCP = %q (error %v)", out, isErr)
		}
	})

	t.Run("read-only server denies the writing tools", func(t *testing.T) {
		in := newInstance(t)
		in.model = ""
		s := startMCP(t, in, "--read-only")
		if names := s.tools(); !containsString(names, "read") {
			t.Errorf("the read-only server lists %v, want read among them", names)
		}
		if out, isErr := s.invoke("write", map[string]any{"path": "denied.txt", "content": "x"}); !isErr {
			t.Errorf("a write through the read-only server was not marked an error: %q", out)
		}
		if out, isErr := s.invoke("bash", map[string]any{"command": "echo x > denied2.txt"}); !isErr {
			t.Errorf("a shell command through the read-only server was not marked an error: %q", out)
		}
		for _, name := range []string{"denied.txt", "denied2.txt"} {
			if _, err := in.workfile(name); err == nil {
				t.Errorf("%s exists although the read-only server refused to create it", name)
			}
		}
	})
}
