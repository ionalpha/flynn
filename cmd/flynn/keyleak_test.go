package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/diag"
	"github.com/ionalpha/flynn/notices"
)

// TestAResolvedKeyNeverReachesOutput: a command that resolves a provider key and then
// fails on the provider's refusal prints neither the key nor any part of it. A dev run's
// output is often saved to a log file, so a key in an error message or a progress line
// is a key on disk. The provider is a local server that refuses every request and
// echoes the key it was sent in the refusal, as some proxies do.
func TestAResolvedKeyNeverReachesOutput(t *testing.T) {
	const key = "sk-flynn-leak-probe-7c2e91d4b05a" // gitleaks:allow (a fake key the test server echoes)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent := r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
		msg, _ := json.Marshal("invalid key: " + sent)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":` + string(msg) + `}}`))
	}))
	t.Cleanup(srv.Close)

	cases := []struct{ provider, keyVar, baseVar, model string }{
		{"anthropic", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "anthropic:claude-opus-4-8"},
		{"openai", "OPENAI_API_KEY", "OPENAI_BASE_URL", "openai:gpt-5"},
	}
	for _, tc := range cases {
		for _, args := range [][]string{{"goal", "tidy the repo"}, {"resume", "some-run"}} {
			t.Run(tc.provider+"/"+args[0], func(t *testing.T) {
				t.Setenv(notices.OffEnv, "1")
				t.Setenv(diag.EnvDir, "")
				t.Setenv(tc.keyVar, key)
				t.Setenv(tc.baseVar, srv.URL)
				keepRunGlobals(t)

				var out, errs bytes.Buffer
				fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
				fs.SetOutput(&errs)
				full := append([]string{"--data-dir", t.TempDir(), "--model", tc.model}, args...)
				code := run(fs, full, &out, &errs)
				if code == 0 {
					t.Fatalf("exit 0 against a provider that refuses every request")
				}
				for name, s := range map[string]string{"stdout": out.String(), "stderr": errs.String()} {
					if strings.Contains(s, key) || strings.Contains(s, key[len(key)-12:]) {
						t.Errorf("%s carries the resolved key", name)
					}
				}
			})
		}
	}
}
