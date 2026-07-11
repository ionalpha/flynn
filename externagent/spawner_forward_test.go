package externagent

import "testing"

// TestSandboxSpawnerForwardBridge covers the bridge-address translation the runner relies
// on, holding on every platform: either the child reaches the host bridge directly (a
// shared network stack), in which case the URL is unchanged and no forward is reported, or
// it reaches an in-namespace address the sandbox forwards to the host one (a confined
// network namespace), in which case the child URL differs and the host address is the
// forward target.
func TestSandboxSpawnerForwardBridge(t *testing.T) {
	sp := NewSandboxSpawner(SandboxConfig{})
	const hostURL = "http://127.0.0.1:5000/mcp"
	child, forwardTo := sp.ForwardBridge(hostURL)

	if forwardTo == "" {
		if child != hostURL {
			t.Errorf("with no forward the child must use the host URL unchanged, got %q", child)
		}
		return
	}
	if child == hostURL {
		t.Errorf("a forwarded child URL must differ from the host URL, got %q", child)
	}
	if forwardTo != "127.0.0.1:5000" {
		t.Errorf("forwardTo must be the host address, got %q", forwardTo)
	}
}

// TestTempEnv confirms the per-episode temp directory is named by every temp variable a CLI
// runtime might read, so the same launch works on Unix (TMPDIR) and Windows (TMP, TEMP).
func TestTempEnv(t *testing.T) {
	env := tempEnv("/scratch/ep")
	for _, want := range []string{"TMPDIR=/scratch/ep", "TMP=/scratch/ep", "TEMP=/scratch/ep"} {
		if !containsEnv(env, want) {
			t.Errorf("tempEnv missing %q in %v", want, env)
		}
	}
}
