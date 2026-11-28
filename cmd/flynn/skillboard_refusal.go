package main

import (
	"strings"

	"github.com/ionalpha/flynn/internal/text"
)

// providerRefusals are how a model provider, or a CLI relaying one, says it will not
// serve a run: no money, no access, or no capacity.
var providerRefusals = []string{
	"no credits remaining", "credit balance", "insufficient_quota", "exceeded your current quota", "billing",
	"rate limit", "rate_limit", "too many requests", "overloaded",
	"invalid api key", "invalid x-api-key", "incorrect api key", "authentication_error", "permission_error", "unauthorized",
	"http 401", "http 403", "http 429", "http 5",
	"api error: 401", "api error: 403", "api error: 429", "api error: 5",
}

// providerRefusal returns the line of msg that says a provider refused the run, or ""
// when none does. Give it only text a CLI or an API client wrote about its own failure,
// never the agent's words: an exercise about rate limits would read as a refusal.
func providerRefusal(msg string) string {
	for _, line := range strings.Split(msg, "\n") {
		if text.ContainsAny(strings.ToLower(line), providerRefusals...) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// apiRefusal is [providerRefusal] for the native arm, whose run error can also quote
// tool output. Only an HTTP status fault from a model API is read.
func apiRefusal(runErr string) string {
	if !strings.Contains(runErr, "_status: ") {
		return ""
	}
	return providerRefusal(runErr)
}

// verifierStarted reports whether the verifier ran at all. 3221225794 is Windows
// failing to initialise a process (0xC0000142), which grades nothing.
func verifierStarted(output string) bool {
	return !strings.HasPrefix(output, "the sandbox could not start:") &&
		!strings.HasPrefix(output, "the command could not run:") &&
		!strings.HasPrefix(output, "exit 3221225794")
}
