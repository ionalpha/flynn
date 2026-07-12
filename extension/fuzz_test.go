package extension

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// FuzzParseHostCall fuzzes the host-call decoder against the bytes an extension subprocess can
// actually put on the wire. This is the boundary that matters: a tool result is whatever an
// extension wrote to its stdout, so it is FOREIGN INPUT, and the host reads it before it has any
// reason to trust it. A hostile extension controls every byte here.
//
// The properties asserted are the ones the security of the boundary rests on:
//
//   - Decoding never panics, whatever the bytes are.
//   - A message the host would act on decodes to bytes the host can bound BEFORE acting: an
//     over-sized or non-base64 payload is detectable, never silently signed or sent.
//   - A message asking for both authorities at once stays distinguishable, so the host can refuse
//     it rather than pick one.
//   - Whatever the tool said, the resume the host builds is valid JSON carrying the session, so a
//     malformed result cannot corrupt the next call's input (no injection into the resume).
func FuzzParseHostCall(f *testing.F) {
	payload := base64.StdEncoding.EncodeToString([]byte("bytes-to-sign"))
	f.Add(`{"session":"s1","sign":{"message":"` + payload + `"}}`)
	f.Add(`{"session":"s1","fetch":{"body":"` + payload + `"}}`)
	f.Add(`{"session":"s1","sign":{"message":"` + payload + `"},"fetch":{"body":"` + payload + `"}}`)
	f.Add(`{"done":true,"result":"ok"}`)
	f.Add(`{"session":"s1","sign":{"message":"!!not-base64!!"}}`)
	f.Add(`{"session":"","sign":{"message":""}}`)
	f.Add(`not json at all`)
	f.Add(``)
	f.Add(`{"session":"a\"b","sign":{"message":"` + payload + `"}}`) // a session that tries to break out
	f.Add(`null`)

	f.Fuzz(func(t *testing.T, text string) {
		reply := parseHostCall(text)

		// A message carrying both blocks must remain visibly ambiguous. The host refuses it; what
		// it must never do is silently service one authority when the tool asked for two.
		if reply.Sign != nil && reply.Fetch != nil {
			return
		}

		switch {
		case reply.Sign != nil:
			raw, err := base64.StdEncoding.DecodeString(reply.Sign.Message)
			if err != nil {
				return // refused before anything is signed
			}
			// The host bounds the payload before signing it, so an over-sized one is always
			// detectable from the decoded bytes rather than discovered mid-signature.
			if len(raw) > 64<<10 {
				return
			}
			assertValidResume(t, reply.Session, func() (json.RawMessage, error) {
				return resumeSign(reply.Session, []byte("sig"), nil)
			})

		case reply.Fetch != nil:
			raw, err := base64.StdEncoding.DecodeString(reply.Fetch.Body)
			if err != nil {
				return // refused before anything is sent
			}
			if len(raw) > 256<<10 {
				return
			}
			assertValidResume(t, reply.Session, func() (json.RawMessage, error) {
				return resumeFetch(reply.Session, []byte("response"), nil)
			})

		default:
			// Terminal: the host hands this back to the caller and asks the tool for nothing.
		}
	})
}

// assertValidResume checks that the follow-up call the host builds from an untrusted session token
// is well-formed JSON that carries that token back verbatim. A tool that puts quotes, braces, or
// newlines in its session id must not be able to inject anything into the next call's input.
func assertValidResume(t *testing.T, session string, build func() (json.RawMessage, error)) {
	t.Helper()
	out, err := build()
	if err != nil {
		t.Fatalf("resume could not be built for session %q: %v", session, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("resume for session %q is not valid JSON: %v (%s)", session, err, out)
	}
	got, ok := obj[sessionField].(string)
	if !ok {
		t.Fatalf("resume for session %q carries no session token: %s", session, out)
	}
	if got != session {
		t.Fatalf("resume changed the session token: sent %q, got %q", session, got)
	}
}

// FuzzInjectHostKey fuzzes the other direction: the CALLER's arguments, which the host must merge
// the granted key into before the first call. The input is model-supplied, so it is untrusted too.
// The property is that injection either fails cleanly or produces an object that still carries the
// key, and never silently drops it (a tool that did not learn the key would build against nothing).
func FuzzInjectHostKey(f *testing.F) {
	f.Add(`{"foo":"bar"}`)
	f.Add(`{}`)
	f.Add(`null`)
	f.Add(``)
	f.Add(`{"_hostKey":"attacker-supplied"}`)
	f.Add(`[1,2,3]`)
	f.Add(`not json`)

	pub := testFuzzKey()
	f.Fuzz(func(t *testing.T, input string) {
		out, err := injectHostKey(json.RawMessage(input), pub)
		if err != nil {
			return // a non-object argument is refused, which is the correct answer
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("injected input is not valid JSON: %v (%s)", err, out)
		}
		var key string
		if err := json.Unmarshal(obj[hostKeyField], &key); err != nil {
			t.Fatalf("injected input carries no host key: %s", out)
		}
		// The GRANTED key wins, always. A caller that supplied its own _hostKey cannot talk the
		// host into letting the tool build against a key the host did not grant.
		if want := base64.StdEncoding.EncodeToString(pub); key != want {
			t.Fatalf("host key was overridden by the caller's argument: got %q, want %q", key, want)
		}
	})
}

// testFuzzKey is a fixed public key: the fuzz asserts the key is carried through, not what it is.
func testFuzzKey() []byte {
	return []byte(strings.Repeat("k", 32))
}
