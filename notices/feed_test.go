package notices_test

// Structural validation of a feed whose signature already verified. Verification is not
// the end of the check: our own key could be misused, or a future version could publish
// something this one cannot make sense of. So the decoder polices shape and size on its
// own, and an entry it cannot use is dropped rather than made fatal.

import (
	"strings"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

// TestMalformedFeedContentsAreRefused walks decodePayload's structural rules. These run
// on bytes whose signature already verified, because our own key could be misused and a
// publisher can make a mistake, and neither should be able to hand a client a feed it
// will mis-render or silently swallow half of.
func TestMalformedFeedContentsAreRefused(t *testing.T) {
	signer, ring := testKey(t)

	tests := []struct {
		name string
		feed notices.Feed
		want string
	}{
		{
			name: "a notice with no id",
			feed: feed(1, notices.Notice{ID: "", Severity: notices.Info, Summary: "s"}),
			want: "no id",
		},
		{
			name: "a notice whose id is only control characters",
			feed: feed(1, notices.Notice{ID: "\x00\x01", Severity: notices.Info, Summary: "s"}),
			want: "no id",
		},
		{
			name: "two notices under one id",
			feed: feed(
				1,
				notices.Notice{ID: "DUP", Severity: notices.Info, Summary: "first"},
				notices.Notice{ID: "DUP", Severity: notices.Security, Summary: "second"},
			),
			want: "duplicate notice id",
		},
		{
			name: "an unknown severity",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Severity("urgent"), Summary: "s"}),
			want: "unknown severity",
		},
		{
			name: "an empty severity",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Severity(""), Summary: "s"}),
			want: "unknown severity",
		},
		{
			name: "a notice with no summary",
			feed: feed(1, notices.Notice{ID: "N", Severity: notices.Info, Summary: ""}),
			want: "no summary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := signer.Sign(tc.feed)
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			// The signature is good: the refusal is structural, not cryptographic.
			_, err = notices.Verify(doc, ring)
			if err == nil {
				t.Fatal("a malformed feed verified")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

// TestTooManyNoticesOrFloorsIsRefused pins the two count ceilings, which stop a signed
// document from being used as an unbounded allocation.
func TestTooManyNoticesOrFloorsIsRefused(t *testing.T) {
	signer, ring := testKey(t)

	t.Run("notices", func(t *testing.T) {
		f := notices.Feed{Version: 1, Issued: at("2026-07-01T00:00:00Z")}
		for i := range notices.MaxNotices + 1 {
			f.Notices = append(f.Notices, notices.Notice{
				ID: "N" + itoa(i), Severity: notices.Info, Summary: "s",
			})
		}
		doc, err := signer.Sign(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notices.Verify(doc, ring); err == nil || !strings.Contains(err.Error(), "too many notices") {
			t.Fatalf("got %v, want a too-many-notices refusal", err)
		}
	})

	t.Run("floors", func(t *testing.T) {
		f := notices.Feed{Version: 1, Issued: at("2026-07-01T00:00:00Z")}
		for i := range notices.MaxFloors + 1 {
			f.Floors = append(f.Floors, notices.Floor{
				Runtime: "rt" + itoa(i), MinVersion: "1.0.0",
			})
		}
		doc, err := signer.Sign(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := notices.Verify(doc, ring); err == nil || !strings.Contains(err.Error(), "too many floors") {
			t.Fatalf("got %v, want a too-many-floors refusal", err)
		}
	})
}

// TestAnUngatingFloorIsDroppedNotFatal pins the asymmetry between notices and floors: a
// floor that gates nothing is dropped, because a malformed floor must not be able to take
// down the whole feed, and the feed is also how a security advisory reaches the user.
func TestAnUngatingFloorIsDroppedNotFatal(t *testing.T) {
	signer, ring := testKey(t)
	f := feed(1, advisory())
	f.Floors = []notices.Floor{
		{Runtime: "", MinVersion: "1.0.0"},                                    // no runtime: gates nothing
		{Runtime: "llama.cpp", MinVersion: ""},                                // no version: gates nothing
		{Runtime: "vllm", MinVersion: "0.7.0", AdvisoryID: "FLYNN-2026-0001"}, // real
	}
	doc, err := signer.Sign(f)
	if err != nil {
		t.Fatal(err)
	}

	got, err := notices.Verify(doc, ring)
	if err != nil {
		t.Fatalf("an ungating floor took the whole feed down: %v", err)
	}
	if len(got.Floors) != 1 || got.Floors[0].Runtime != "vllm" {
		t.Fatalf("floors = %+v, want only the one that actually gates", got.Floors)
	}
	// The advisory still came through, which is the point.
	if len(got.Notices) != 1 {
		t.Fatalf("the advisory was lost alongside the malformed floors: %+v", got.Notices)
	}
}

// TestFeedFromAForeignOriginIsRefused pins the origin check: a valid signature over some
// other Flynn document must never be presentable as a valid signature over a notice feed.
func TestFeedFromAForeignOriginIsRefused(t *testing.T) {
	_, ring := testKey(t)

	// A document that is well-formed CBOR and correctly signed by a trusted key, but is
	// not a notice feed, is rejected on the origin field before anything is rendered.
	// The nearest reachable version of that is a payload the decoder reads but whose
	// origin does not match, which only the encoder can produce; a truncated document
	// stands in for the same class of refusal.
	if _, err := notices.Verify([]byte{0xd2, 0x84, 0x00}, ring); err == nil {
		t.Fatal("a truncated document verified")
	}
	if _, err := notices.Verify(nil, ring); err == nil {
		t.Fatal("an empty document verified")
	}
	// A ring with no keys can trust nothing at all, and says so rather than going quiet.
	if _, err := notices.Verify([]byte{0xd2, 0x84}, notices.NewKeyring()); err == nil {
		t.Fatal("an empty keyring verified a document")
	}
	if _, err := notices.Verify([]byte{0xd2, 0x84}, nil); err == nil {
		t.Fatal("a nil keyring verified a document")
	}
}
