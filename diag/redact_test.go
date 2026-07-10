package diag

import (
	"slices"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

func TestRedactArgs(t *testing.T) {
	const r = secret.Redacted

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "nil argv",
			args: nil,
			want: nil,
		},
		{
			name: "the program path and a bare subcommand survive",
			args: []string{"/usr/bin/flynn", "runs"},
			want: []string{"/usr/bin/flynn", "runs"},
		},
		{
			name: "an objective is free user text",
			args: []string{"flynn", "goal", "delete the customer table"},
			want: []string{"flynn", "goal", r},
		},
		{
			name: "every word of an unquoted objective is redacted",
			args: []string{"flynn", "goal", "delete", "the", "table"},
			want: []string{"flynn", "goal", r, r, r},
		},
		{
			name: "flags before the subcommand keep their values, the objective does not",
			args: []string{"flynn", "--profile", "/tmp/b", "--model", "anthropic:opus", "goal", "ship it"},
			want: []string{"flynn", "--profile", "/tmp/b", "--model", "anthropic:opus", "goal", r},
		},
		{
			name: "an auth subcommand's arguments are a credential",
			args: []string{"flynn", "auth", "set", "anthropic"},
			want: []string{"flynn", "auth", r, r},
		},
		{
			name: "a sensitive flag's inline value goes, its name stays",
			args: []string{"flynn", "serve", "--telegram-token=8123:AAH-secret"},
			want: []string{"flynn", "serve", "--telegram-token=" + r},
		},
		{
			name: "a sensitive flag's separate value goes",
			args: []string{"flynn", "serve", "--telegram-token", "8123:AAH-secret"},
			want: []string{"flynn", "serve", "--telegram-token", r},
		},
		{
			name: "a boolean sensitive flag does not swallow the next flag",
			args: []string{"flynn", "--no-auth", "--verbose", "runs"},
			want: []string{"flynn", "--no-auth", "--verbose", "runs"},
		},
		{
			name: "single-dash flags are handled like double-dash ones",
			args: []string{"flynn", "-api-key", "sk-live-abcdefghijklmnop", "runs"},
			want: []string{"flynn", "-api-key", r, "runs"},
		},
		{
			// The value here is a low-entropy placeholder on purpose: the repo's secret
			// scan reads a realistic one next to a key-shaped flag name as a real leak.
			name: "a sensitive flag name is matched case-insensitively",
			args: []string{"flynn", "--API-KEY", "PLACEHOLDER-VALUE", "runs"},
			want: []string{"flynn", "--API-KEY", r, "runs"},
		},
		{
			name: "a bare credential is caught by its prefix even with no flag naming it",
			args: []string{"flynn", "models", "fetch", "hf_abcdefghijklmnop"},
			want: []string{"flynn", "models", "fetch", r},
		},
		{
			name: "an ordinary value that merely starts like a credential is kept",
			args: []string{"flynn", "get", "sk-1"},
			want: []string{"flynn", "get", "sk-1"},
		},
		{
			name: "a trailing sensitive flag with no value does not read past the end",
			args: []string{"flynn", "serve", "--telegram-token"},
			want: []string{"flynn", "serve", "--telegram-token"},
		},
		{
			name: "a data dir is not a secret",
			args: []string{"flynn", "--data-dir", "/home/u/.flynn", "runs"},
			want: []string{"flynn", "--data-dir", "/home/u/.flynn", "runs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactArgs(tt.args)
			if !slices.Equal(got, tt.want) {
				t.Errorf("RedactArgs(%q)\n got %q\nwant %q", tt.args, got, tt.want)
			}
		})
	}
}

// RedactArgs must never hand back the caller's slice, since the manifest outlives
// the argv the caller passed and a shared backing array would let one mutate the other.
func TestRedactArgsDoesNotAliasItsInput(t *testing.T) {
	args := []string{"flynn", "goal", "secret objective"}
	got := RedactArgs(args)
	if &got[0] == &args[0] {
		t.Fatal("RedactArgs returned a slice aliasing its input")
	}
	if args[2] != "secret objective" {
		t.Errorf("RedactArgs mutated its input: args[2] = %q", args[2])
	}
}
