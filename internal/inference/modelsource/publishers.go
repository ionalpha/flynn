package modelsource

import "strings"

// recognizedPublishers are hub owners that release the weights they are named for, as
// opposed to reuploaders. Membership raises a hub model from untrusted to semi-trusted,
// so it can run under kernel confinement instead of being refused. The list is small
// and curated so an unlisted owner stays untrusted. Keys are lower case because hub
// owners match case-insensitively.
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
// extra adds owners a curator already vouched for, such as the catalog's publishers.
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
