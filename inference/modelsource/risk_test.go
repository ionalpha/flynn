package modelsource

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/sandbox"
)

func TestDescribeRiskMatchesGate(t *testing.T) {
	// The isolation shown to a user must be exactly what the gate enforces for the trust
	// level, so the surface never understates the requirement.
	src := Source{Kind: KindHuggingFace, Raw: "hf:rando/x", Owner: "rando", Repo: "x"}
	class := Classify(src, func(string) bool { return false }) // untrusted
	rs := DescribeRisk(src, class, IntegrityUnverified)

	if rs.Trust != sandbox.TrustUntrusted {
		t.Fatalf("trust = %v, want untrusted", rs.Trust)
	}
	if rs.Required != sandbox.Required(sandbox.TrustUntrusted) {
		t.Fatalf("required containment %v does not match the gate %v", rs.Required, sandbox.Required(sandbox.TrustUntrusted))
	}
	if !rs.Risky() {
		t.Fatal("an untrusted source must be risky")
	}
	joined := strings.Join(rs.Lines(), "\n")
	for _, want := range []string{"untrusted", "microvm", "unverified", "no network"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("risk lines missing %q:\n%s", want, joined)
		}
	}
}

func TestRiskTrustedNotRisky(t *testing.T) {
	src := Source{Kind: KindCatalog, Raw: "id", CatalogID: "id"}
	rs := DescribeRisk(src, Classify(src, nil), IntegrityPinned)
	if rs.Risky() {
		t.Fatal("a trusted catalog model must not be risky")
	}
	if !strings.Contains(strings.Join(rs.Lines(), "\n"), "pinned digest") {
		t.Fatal("a pinned catalog model must surface its verified integrity")
	}
}

func TestIntegrityString(t *testing.T) {
	cases := map[Integrity]string{
		IntegrityPinned:     "pinned digest",
		IntegrityTOFU:       "first use",
		IntegrityUnverified: "unverified",
	}
	for integ, want := range cases {
		if !strings.Contains(integ.String(), want) {
			t.Fatalf("Integrity(%d).String() = %q, want to contain %q", integ, integ.String(), want)
		}
	}
}
