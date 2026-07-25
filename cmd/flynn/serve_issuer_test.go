package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/controlplane"
)

// farFuture is a fixed expiry well beyond any test's wall clock, chosen without reading
// the clock so the test carries no time.Now. A token stamped with it is never expired
// when the server (on the system clock) verifies it.
var farFuture = time.Unix(1<<40, 0)

// TestServeIssuerAndTokenAreMutuallyExclusive: a static bearer and an issuer key are two
// different trust models (one capped at read scope, one accepting scope-attenuated
// delegated tokens). Supplying both is a configuration error, refused before anything is
// opened rather than one silently winning.
func TestServeIssuerAndTokenAreMutuallyExclusive(t *testing.T) {
	dataDir, spec := serveEnv(t)
	issuer, err := controlplane.GenerateIdentity()
	if err != nil {
		t.Fatalf("generate issuer: %v", err)
	}
	args := []string{
		"--api-addr", "127.0.0.1:0",
		"--api-token", "operator-token",
		"--api-issuer", controlplane.PrincipalID(issuer.Public()),
	}
	err = runServeContext(context.Background(), args, spec, dataDir)
	if err == nil {
		t.Fatal("expected --api-token and --api-issuer together to be refused")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want it to name the mutual exclusion", err)
	}
}

// TestServeIssuerRejectsAMalformedKey: --api-issuer takes a self-certifying principal id
// (ed25519:<base64>). A value that is not one fails at startup, not with every request
// silently unauthenticated.
func TestServeIssuerRejectsAMalformedKey(t *testing.T) {
	dataDir, spec := serveEnv(t)
	args := []string{"--api-addr", "127.0.0.1:0", "--api-issuer", "not-a-key"}
	err := runServeContext(context.Background(), args, spec, dataDir)
	if err == nil {
		t.Fatal("expected a malformed --api-issuer to be refused")
	}
	if !strings.Contains(err.Error(), "api-issuer") {
		t.Fatalf("error = %v, want it to name the flag", err)
	}
}

// TestServeIssuerAcceptsADelegatedToken: with --api-issuer set, the control-plane API
// authenticates capability tokens issued under that key and rejects everything else. A
// token the operator issued verifies offline against the issuer public key and is let
// through; a forged bearer is unauthenticated. This is the wiring that lets an enrolling
// operator drive the box with a scope-attenuated token instead of shipping it a secret.
func TestServeIssuerAcceptsADelegatedToken(t *testing.T) {
	dataDir, spec := serveEnv(t)
	addr := freeLoopbackAddr(t)

	issuer, err := controlplane.GenerateIdentity()
	if err != nil {
		t.Fatalf("generate issuer: %v", err)
	}
	audience, err := controlplane.GenerateIdentity()
	if err != nil {
		t.Fatalf("generate audience: %v", err)
	}
	// A read-scoped token the operator mints for the audience it is driving from.
	tok, err := controlplane.Issue(issuer, audience.Public(), controlplane.ScopeRead, capability.AllowAll(), farFuture)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- runServeContext(ctx, []string{"--api-addr", addr, "--api-issuer", controlplane.PrincipalID(issuer.Public())}, spec, dataDir)
	}()
	waitForListener(t, addr, errc)

	// A forged bearer is not issued under the trusted key: it is unauthenticated.
	if got := getStatus(t, addr, "forged-token"); got != http.StatusUnauthorized {
		t.Fatalf("forged token: status = %d, want %d", got, http.StatusUnauthorized)
	}
	// The delegated token verifies against the issuer key and is authenticated: whatever
	// the read route answers, it is not a 401.
	if got := getStatus(t, addr, tok); got == http.StatusUnauthorized {
		t.Fatal("delegated token was rejected; --api-issuer did not wire the delegation authenticator")
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("a cancelled serve must stop cleanly, got %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop after its context was cancelled")
	}
}

// getStatus performs a read request against the served control-plane API with the given
// bearer and returns the HTTP status, so a test can tell an authenticated request from a
// rejected one without depending on the resource body.
func getStatus(t *testing.T, addr, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/instance", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
