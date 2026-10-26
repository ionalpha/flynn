package localmodel

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// urlText matches a URL inside free text, which is where a credential, a signed query
// string or a token in a path ends up when a downloader or a runtime reports a failure.
var urlText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>)\]]+`)

// redactText reduces every URL in s to its scheme and host and replaces each secret with
// a marker. The path and query are dropped because they are where a signed download
// link carries its credential.
func redactText(s string, secrets ...string) string {
	s = urlText.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "<url>"
		}
		return u.Scheme + "://" + u.Host + "/..."
	})
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	return s
}

// redactedError carries a message already passed through [redactText]. It keeps the
// original cause for [errors.Is] but does not unwrap to it: unwrapping would hand a caller
// that prints the chain the unredacted text.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

// Is reports whether the original error matched target.
func (e *redactedError) Is(target error) bool { return errors.Is(e.cause, target) }

// fail returns err with its message redacted, including every token this host knows of.
func (h *Host) fail(err error) error {
	if err == nil {
		return nil
	}
	var r *redactedError
	if errors.As(err, &r) {
		return err
	}
	return &redactedError{msg: redactText(err.Error(), h.knownTokens()...), cause: err}
}

func (h *Host) knownTokens() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.tokens))
	for _, t := range h.tokens {
		out = append(out, t)
	}
	return out
}

// dataDirPath joins a path under the data directory.
func (h *Host) dataDirPath(elem ...string) string {
	return filepath.Join(append([]string{h.dataDir}, elem...)...)
}

func redactLines(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = redactText(l)
	}
	return out
}
