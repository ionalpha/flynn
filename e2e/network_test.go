package e2e

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultShellNetworkPostureMatchesWhatHappened has the model fetch from a listener on
// the host's loopback under the default policy, and holds the recorded posture to what the
// fetch did. Linux and macOS deny the network, so the fetch fails and the record says denied.
// Windows cannot deny it: the fetch succeeds, and the record must say the network was open
// because of the platform, rather than claiming a denial that did not happen. There,
// --require-network-denial refuses the command instead.
func TestDefaultShellNetworkPostureMatchesWhatHappened(t *testing.T) {
	const marker = "listener-reached-e2e-7c20"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, marker)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	url := "http://" + ln.Addr().String() + "/"
	fetch := "curl -s --max-time 5 " + url + "; echo fetch-attempted"
	if runtime.GOOS == "windows" {
		fetch = "curl.exe -s --max-time 5 " + url + " & echo fetch-attempted"
	}
	script := func() *fakeOpenAI {
		return newFakeOpenAIQueue(t, toolCall("c", "bash", `{"command":"`+jsonEscape(fetch)+`"}`), finalText("done"))
	}
	in := newInstance(t)

	fake := script()
	res := in.withModel(fake).run("-no-learn", "goal", "fetch from the listener")
	requireExit(t, res, 0, "goal fetching from a local listener")
	out := toolOutputs(t, fake)
	requireContains(t, out, "fetch-attempted", "the fetch command ran")

	path, exp := in.export(in.runID(res))
	requireExit(t, exp, 0, "spine export")
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	posture := recordedPosture(t, record)

	reached := strings.Contains(out, marker)
	if runtime.GOOS != "windows" {
		if reached {
			t.Fatalf("the shell reached a host listener under the default policy: %q", out)
		}
		if posture["network"] != "denied" {
			t.Fatalf("posture = %v, want the network recorded denied", posture)
		}
		if strings.Contains(res.stderr, "cannot deny the network") {
			t.Errorf("a platform that denies the network announced it could not:\n%s", res.stderr)
		}
		return
	}
	if !reached {
		t.Fatalf("Windows cannot deny the network, so the fetch should have reached the listener; got %q", out)
	}
	if posture["network"] != "open" || posture["network_reason"] != "platform" {
		t.Fatalf("posture = %v, want the network recorded open because of the platform", posture)
	}
	if !strings.Contains(res.stderr, "cannot deny the network") {
		t.Errorf("the run did not tell the operator the network is open:\n%s", res.stderr)
	}

	strict := script()
	res = in.withModel(strict).run("-no-learn", "-require-network-denial", "goal", "fetch from the listener")
	out = toolOutputs(t, strict)
	if strings.Contains(out, marker) || strings.Contains(out, "fetch-attempted") {
		t.Fatalf("--require-network-denial ran the command: %q", out)
	}
	requireContains(t, out, "cannot deny the network", "the strict refusal names why")
	if strings.Contains(res.stderr, "cannot deny the network to the commands") {
		t.Errorf("the strict run announced an open network it never granted:\n%s", res.stderr)
	}
}
