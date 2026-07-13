package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/internal/fetch"
)

// serving stands up a TLS server for one handler, so a test can serve exactly the
// listing it wants to be refused for.
func serving(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	return s
}

// updaterAgainst wires an Updater to one server, with a running binary that exists so
// the listing is what fails rather than the install target.
func updaterAgainst(t *testing.T, s *httptest.Server, current string) *Updater {
	t.Helper()
	client := s.Client()
	client.Timeout = 30 * time.Second
	exe := installedFlynn(t, []byte("flynn"))

	return New(t.TempDir(), func(u *Updater) {
		u.http = client
		u.fetcher = fetch.New(fetch.WithHTTPClient(client))
		u.listingURL = s.URL + "/releases"
		u.downloadBase = s.URL + "/download/"
		u.version = current
		u.exe = func() (string, error) { return exe, nil }
		u.clock = clock.NewManual(time.Unix(1783492338, 0).UTC())
	})
}

// The listing is fetched over a hardened transport and parsed strictly. Everything it
// can do wrong ends as a refusal with a code, never as an empty list that reads like
// "you are up to date".
func TestListRefusesAListingItCannotTrust(t *testing.T) {
	t.Run("an HTTP error is not an empty listing", func(t *testing.T) {
		s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		})
		_, err := updaterAgainst(t, s, "v0.1.0").List(context.Background())
		if err == nil {
			t.Fatal("an HTTP error was read as a release listing")
		}
		if !strings.Contains(err.Error(), "HTTP 429") {
			t.Fatalf("err = %v, want it to name the status", err)
		}
		// A server that is having a bad day is worth retrying; it is not a reason to stop.
		if fault.Classify(err) != fault.Transient {
			t.Fatalf("class = %v, want transient", fault.Classify(err))
		}
	})

	t.Run("a listing that is not JSON", func(t *testing.T) {
		s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>a proxy's login page</html>"))
		})
		_, err := updaterAgainst(t, s, "v0.1.0").List(context.Background())
		if err == nil {
			t.Fatal("a listing that is not JSON was accepted")
		}
		if codeOf(t, err) != CodeListing {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeListing)
		}
	})

	t.Run("a listing with no releases in it", func(t *testing.T) {
		s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		})
		_, err := updaterAgainst(t, s, "v0.1.0").List(context.Background())
		if codeOf(t, err) != CodeNoRelease {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeNoRelease)
		}
	})

	t.Run("a listing offering only drafts and tags that are not versions", func(t *testing.T) {
		s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[
				{"tag_name":"v9.9.9","draft":true,"prerelease":false},
				{"tag_name":"nightly","draft":false,"prerelease":false},
				{"tag_name":"v1.0","draft":false,"prerelease":false}
			]`))
		})
		// A draft is not published, and a tag that is not a version is not a release this
		// binary knows how to be. Neither may become an upgrade candidate.
		_, err := updaterAgainst(t, s, "v0.1.0").List(context.Background())
		if codeOf(t, err) != CodeNoRelease {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeNoRelease)
		}
	})

	t.Run("a listing served over an unencrypted transport", func(t *testing.T) {
		s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[]`))
		})
		u := updaterAgainst(t, s, "v0.1.0")
		u.listingURL = "http://example.invalid/releases"
		_, err := u.List(context.Background())
		if err == nil {
			t.Fatal("a listing was fetched over plain HTTP")
		}
		if !strings.Contains(err.Error(), "unencrypted transport") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("a listing URL that is not a URL", func(t *testing.T) {
		s := serving(t, func(_ http.ResponseWriter, _ *http.Request) {})
		u := updaterAgainst(t, s, "v0.1.0")
		u.listingURL = "https://example.invalid/\x7f"
		if _, err := u.List(context.Background()); err == nil {
			t.Fatal("a request was built from a URL that cannot be one")
		}
	})

	t.Run("a server that cannot be reached", func(t *testing.T) {
		s := serving(t, func(_ http.ResponseWriter, _ *http.Request) {})
		u := updaterAgainst(t, s, "v0.1.0")
		s.Close() // the listener is gone before the request is made
		_, err := u.List(context.Background())
		if err == nil {
			t.Fatal("a listing was read from a server that is not there")
		}
		if fault.Classify(err) != fault.Transient {
			t.Fatalf("class = %v, want transient", fault.Classify(err))
		}
	})
}

// A body is read one byte past its ceiling, so an over-long document is refused rather
// than truncated into something that parses as if it were complete. A listing that is
// cut off at the ceiling would parse as a shorter listing, which is a listing that hides
// releases.
func TestGetRefusesABodyOverTheCeiling(t *testing.T) {
	s := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	})
	u := updaterAgainst(t, s, "v0.1.0")

	if _, err := u.get(context.Background(), s.URL+"/releases", 16); err == nil {
		t.Fatal("a body over the ceiling was accepted")
	} else if !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("err = %v", err)
	}

	// A body inside the ceiling is returned whole, ceiling-sized bodies included.
	raw, err := u.get(context.Background(), s.URL+"/releases", 64)
	if err != nil {
		t.Fatalf("a body exactly at the ceiling was refused: %v", err)
	}
	if len(raw) != 64 {
		t.Fatalf("read %d bytes, want 64", len(raw))
	}
}

// The listing arrives in whatever order the server felt like, and the running version is
// whatever this binary was built as. List has to put those in order and say which one is
// already installed, because every later decision is made on the first entry.
func TestListOrdersReleasesNewestFirst(t *testing.T) {
	o := newOrigin(t)
	o.listRelease("v0.2.0", "v0.10.0", "v0.2.0-rc.1", "v0.9.0")
	u := o.updater(t, "v0.9.0", installedFlynn(t, []byte("flynn")))

	releases, err := u.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got []string
	for _, r := range releases {
		got = append(got, r.Version.String())
	}
	want := []string{"v0.10.0", "v0.9.0", "v0.2.0", "v0.2.0-rc.1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for _, r := range releases {
		if r.Current != (r.Version.String() == "v0.9.0") {
			t.Errorf("%s: current = %v", r.Version, r.Current)
		}
		if r.Version.String() == "v0.2.0-rc.1" && !r.Prerelease {
			t.Error("a version with a prerelease suffix was not marked as one")
		}
	}
	if newest := newestOf(releases); newest.String() != "v0.10.0" {
		t.Fatalf("newest = %s, want v0.10.0", newest)
	}
}

// What the operator asked for decides which release is chosen, and asking for one that
// is not on offer is a refusal rather than a silent substitution of a different version.
func TestSelectTarget(t *testing.T) {
	releases := []Release{
		{Version: mustVersion(t, "v0.3.0-rc.2"), Prerelease: true},
		{Version: mustVersion(t, "v0.2.0")},
		{Version: mustVersion(t, "v0.1.0")},
	}

	tests := []struct {
		name     string
		releases []Release
		req      Request
		want     string
		wantErr  string
	}{
		{name: "the newest stable release by default", releases: releases, want: "v0.2.0"},
		{
			name: "a prerelease only when it is asked for", releases: releases,
			req: Request{AllowPrerelease: true}, want: "v0.3.0-rc.2",
		},
		{
			name: "an exact version wins over the prerelease rule", releases: releases,
			req: Request{To: "v0.3.0-rc.2"}, want: "v0.3.0-rc.2",
		},
		{
			name: "a version that is not on offer", releases: releases,
			req: Request{To: "v0.4.0"}, wantErr: "there is no release v0.4.0",
		},
		{
			name: "a request that is not a version at all", releases: releases,
			req: Request{To: "latest"}, wantErr: "is not a version",
		},
		{
			name:     "nothing but prereleases, and none asked for",
			releases: []Release{{Version: mustVersion(t, "v0.3.0-rc.2"), Prerelease: true}},
			wantErr:  "no stable releases",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectTarget(tc.releases, tc.req)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("selected %s, want a refusal", got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				if codeOf(t, err) != CodeNoRelease {
					t.Fatalf("code = %q, want %q", codeOf(t, err), CodeNoRelease)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectTarget: %v", err)
			}
			if got.String() != tc.want {
				t.Fatalf("selected %s, want %s", got, tc.want)
			}
		})
	}
}

// A plan that would install what is already running says so, which is what lets the
// command report "up to date" rather than performing a rename for no reason.
func TestPlanUpToDate(t *testing.T) {
	same := Plan{Current: mustVersion(t, "v0.2.0"), Target: mustVersion(t, "v0.2.0")}
	if !same.UpToDate() {
		t.Error("a plan targeting the running version is not reported as up to date")
	}
	// The versions are compared as versions, not as text, because the tag and the stamped
	// version are not always spelled the same way.
	spelled := Plan{Current: mustVersion(t, "0.2.0"), Target: mustVersion(t, "v0.2.0")}
	if !spelled.UpToDate() {
		t.Error("two spellings of one version were read as an upgrade")
	}
	newer := Plan{Current: mustVersion(t, "v0.2.0"), Target: mustVersion(t, "v0.3.0")}
	if newer.UpToDate() {
		t.Error("an upgrade was reported as up to date")
	}
}

// The newest release ever offered is remembered so a listing that later goes backwards
// can be noticed. The memory only ever rises: a listing that offers older releases must
// not be able to lower the mark that would have caught it.
func TestRecordSeenOnlyEverRises(t *testing.T) {
	o := newOrigin(t)
	u := o.updater(t, "v0.1.0", installedFlynn(t, []byte("flynn")))

	u.RecordSeen([]Release{{Version: mustVersion(t, "v0.4.0")}, {Version: mustVersion(t, "v0.2.0")}})
	if st := loadState(u.dataDir); st.NewestSeen != "v0.4.0" {
		t.Fatalf("newest seen = %q, want v0.4.0", st.NewestSeen)
	}

	u.RecordSeen([]Release{{Version: mustVersion(t, "v0.2.0")}})
	st := loadState(u.dataDir)
	if st.NewestSeen != "v0.4.0" {
		t.Fatalf("a listing that went backwards lowered the mark to %q", st.NewestSeen)
	}
	if st.LastCheck.IsZero() {
		t.Error("the time of the last look at the listing was not recorded")
	}
	// And the listing that went backwards is now something the state can name.
	if prev, stale := st.staleListing(mustVersion(t, "v0.2.0")); !stale || prev != "v0.4.0" {
		t.Fatalf("staleListing = %q, %v; want v0.4.0, true", prev, stale)
	}
}

// The asset a machine installs is named from its own platform. A release that published
// no archive for this platform is a release this machine cannot install, and it says so
// rather than reaching for something that was built for another one.
func TestCheckRefusesWhenTheReleaseHasNoAssetForThisPlatform(t *testing.T) {
	o := newOrigin(t)
	o.listRelease(goldenTag)
	o.assets[goldenTag+"/flynn.intoto.jsonl"] = goldenBundle(t)

	u := o.updater(t, "v0.1.2", installedFlynn(t, []byte("flynn")))
	u.goarch = "mips64" // a platform the release workflow has never built for

	_, err := u.Check(context.Background(), Request{AllowPrerelease: true})
	if err == nil {
		t.Fatal("a release with no archive for this platform was planned as an upgrade")
	}
	if !strings.Contains(err.Error(), u.assetName()) {
		t.Fatalf("err = %v, want it to name the asset that is missing", err)
	}
}

// The install target is resolved before the plan is handed back, so a binary that cannot
// be replaced is refused at plan time rather than after the download.
func TestCheckRefusesWhenTheRunningBinaryCannotBeFound(t *testing.T) {
	o := newOrigin(t)
	o.listRelease(goldenTag)
	o.assets[goldenTag+"/flynn.intoto.jsonl"] = goldenBundle(t)

	u := o.updater(t, "v0.1.2", "")
	u.exe = func() (string, error) { return "", errors.New("this process has no path") }

	_, err := u.Check(context.Background(), Request{AllowPrerelease: true})
	if err == nil {
		t.Fatal("an upgrade was planned for a binary that cannot be located")
	}
	if codeOf(t, err) != CodeInstall {
		t.Fatalf("code = %q, want %q", codeOf(t, err), CodeInstall)
	}
}

// A binary built from a checkout has no released version to be newer than, and no signed
// provenance ever named it. Upgrading one would throw away the developer's own build.
// This also proves an Updater built with no options at all refuses it, which is the one
// that ships.
func TestUpgradeRefusesASourceBuildOnADefaultUpdater(t *testing.T) {
	u := New(t.TempDir())
	if u.http == nil || u.fetcher == nil {
		t.Fatal("a default Updater has no transport")
	}
	u.version = "0.0.0-dev+abc123"

	// The refusal fires before the listing is read, so this never touches the network.
	_, err := u.Check(context.Background(), Request{})
	if err == nil {
		t.Fatal("a source build tried to upgrade itself")
	}
	if codeOf(t, err) != CodeDevBuild {
		t.Fatalf("code = %q, want %q", codeOf(t, err), CodeDevBuild)
	}
}

// Apply is the half that writes. Everything it can be handed that is not installable has
// to fail without touching the binary that is already there.
func TestApplyRefusals(t *testing.T) {
	exe := installedFlynn(t, []byte("the old binary"))
	tgt, err := resolveTarget(exe)
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}

	t.Run("an archive whose bytes are not an archive", func(t *testing.T) {
		// The download is honest: the digest is the digest of exactly these bytes, so the
		// pinning passes and the failure lands in the extractor, where it belongs.
		garbage := []byte("this passed every signature check and is still not an archive")
		o := newOrigin(t)
		u := o.updater(t, "v0.1.2", exe)
		asset := u.assetName()
		o.assets["v0.9.9/"+asset] = garbage

		p := Plan{
			Target:  mustVersion(t, "v0.9.9"),
			Asset:   asset,
			Digest:  sha256hex(garbage),
			URL:     o.server.URL + "/download/v0.9.9/" + asset,
			Path:    tgt.Path,
			install: tgt,
		}
		applyErr := u.Apply(context.Background(), p)
		if applyErr == nil {
			t.Fatal("an archive that does not parse was installed")
		}
		if codeOf(t, applyErr) != CodeArchive {
			t.Fatalf("code = %q, want %q", codeOf(t, applyErr), CodeArchive)
		}
		if got, _ := os.ReadFile(exe); string(got) != "the old binary" {
			t.Fatal("a refused upgrade damaged the installed binary")
		}
		entries, err := os.ReadDir(filepath.Dir(exe))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), stagedPrefix) {
				t.Errorf("a failed upgrade left %s behind", e.Name())
			}
		}
	})

	t.Run("a download the server does not serve", func(t *testing.T) {
		o := newOrigin(t)
		u := o.updater(t, "v0.1.2", exe)
		p := Plan{
			Target:  mustVersion(t, "v0.9.9"),
			Asset:   u.assetName(),
			Digest:  strings.Repeat("00", 32),
			URL:     o.server.URL + "/download/v0.9.9/" + u.assetName(),
			Path:    tgt.Path,
			install: tgt,
		}
		if err := u.Apply(context.Background(), p); err == nil {
			t.Fatal("a release asset that is not there was installed")
		}
		if got, _ := os.ReadFile(exe); string(got) != "the old binary" {
			t.Fatal("a refused upgrade damaged the installed binary")
		}
	})

	t.Run("a destination directory that is not there", func(t *testing.T) {
		o := newOrigin(t)
		u := o.updater(t, "v0.1.2", exe)
		gone := installTarget{
			Path: filepath.Join(t.TempDir(), "gone", binaryFor(runtime.GOOS)),
			Dir:  filepath.Join(t.TempDir(), "gone"),
			Mode: 0o755,
		}
		p := Plan{Target: mustVersion(t, "v0.9.9"), Asset: u.assetName(), install: gone, Path: gone.Path}
		if err := u.Apply(context.Background(), p); err == nil {
			t.Fatal("a binary was staged into a directory that does not exist")
		} else if codeOf(t, err) != CodeInstall {
			t.Fatalf("code = %q, want %q", codeOf(t, err), CodeInstall)
		}
	})
}

// The archive and the binary inside it are named from this process's own platform, never
// from anything a server said.
func TestTheBinaryNameFollowsThePlatform(t *testing.T) {
	for goos, want := range map[string]string{"windows": "flynn.exe", "linux": "flynn", "darwin": "flynn"} {
		u := &Updater{goos: goos, goarch: "amd64"}
		if got := u.binaryName(); got != want {
			t.Errorf("binaryName on %s = %q, want %q", goos, got, want)
		}
		if got, want := u.assetName(), fmt.Sprintf("flynn_%s_amd64", goos); !strings.HasPrefix(got, want) {
			t.Errorf("assetName on %s = %q, want it to start with %q", goos, got, want)
		}
	}
}
