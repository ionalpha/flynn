package modelsource

import "strings"

// recognizedPublishers are model-hub owners treated as first-party publishers: the orgs
// that release the weights they are named for, rather than reuploads of someone else's.
// Membership raises a hub model from untrusted to semi-trusted, so it may run under
// kernel confinement instead of being refused for want of the strong tier. The list is
// deliberately small and curated; an unlisted owner is untrusted by default, never the
// other way around. Matching is case-insensitive because hub owners are.
var recognizedPublishers = map[string]bool{
	"qwen":          true,
	"meta-llama":    true,
	"mistralai":     true,
	"google":        true,
	"microsoft":     true,
	"deepseek-ai":   true,
	"huggingfacetb": true,
	"allenai":       true,
	"nvidia":        true,
	"ibm-granite":   true,
	"tiiuae":        true,
	"cohereforai":   true,
}

// KnownPublisher reports whether a hub owner is a recognized first-party publisher.
// Extra owners (for example the publishers already named in the embedded catalog) can be
// supplied so a source a curator already vouched for is recognized too.
func KnownPublisher(owner string, extra ...string) bool {
	o := strings.ToLower(strings.TrimSpace(owner))
	if o == "" {
		return false
	}
	if recognizedPublishers[o] {
		return true
	}
	for _, e := range extra {
		if strings.ToLower(strings.TrimSpace(e)) == o {
			return true
		}
	}
	return false
}
