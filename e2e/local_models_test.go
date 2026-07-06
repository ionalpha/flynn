package e2e

import (
	"strings"
	"testing"
)

// TestModelCatalogBrowse asserts the offline discovery surface a user hits first: the
// catalog lists models with a trust column, and the api/local filters partition it. This
// is the "browse what I can run" step, and it works with no network.
func TestModelCatalogBrowse(t *testing.T) {
	in := newInstance(t)

	all := in.run("models")
	requireExit(t, all, 0, "models")
	requireContains(t, all.stdout, "catalog", "catalog header")
	requireContains(t, all.stdout, "TRUST", "trust column present")

	api := in.run("models", "-api")
	requireExit(t, api, 0, "models -api")
	local := in.run("models", "-local")
	requireExit(t, local, 0, "models -local")

	// The filters partition the catalog: the local-only listing carries local models
	// (the qwen family) and the api-only listing does not.
	if strings.Contains(api.stdout, "qwen") {
		t.Fatalf("api filter leaked a local model:\n%s", api.stdout)
	}
	if !strings.Contains(local.stdout, "qwen") {
		t.Fatalf("local filter showed no local models:\n%s", local.stdout)
	}
}

// TestModelFetchRefusesUnknownSource asserts the trust gate on the fetch path: a user
// cannot pull weights for a model that is not in the signed catalog. Only a known,
// blessed entry is fetchable, so an arbitrary (untrusted) model id is refused before any
// download, with a distinct non-zero exit. This is the offline half of model-source
// trust; the digest-pinned download itself is exercised only in the opt-in network lane.
func TestModelFetchRefusesUnknownSource(t *testing.T) {
	in := newInstance(t)
	res := in.run("models", "fetch", "definitely-not-a-real-model-xyz")
	requireExit(t, res, 1, "fetch unknown model")
	requireContains(t, res.combined(), "not in the catalog", "unknown model refused as off-catalog")
}

// TestModelsCheckRuntimes asserts the runtime inventory command reports the local
// inference runtimes and their install state without needing the network, so a user can
// see what their box has before fetching anything.
func TestModelsCheckRuntimes(t *testing.T) {
	in := newInstance(t)
	res := in.run("models", "check")
	requireExit(t, res, 0, "models check")
	requireContains(t, res.combined(), "runtime", "reports local inference runtimes")
}
