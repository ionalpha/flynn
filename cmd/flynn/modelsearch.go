package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/huggingface"
)

// runModelSearch implements `flynn models search <query>`: find models on the Hugging
// Face Hub and print each candidate as a ready-to-bless reference. It is the discovery
// step that precedes bless: rather than knowing a repo id in advance, a maintainer
// searches by keyword, sees the popular matches with the signals that matter (downloads,
// likes, pipeline, and whether the weights are a safe-to-load format), and copies one
// `hf:owner/name` line straight into `flynn models bless`.
//
// It reads metadata only, over the same hardened public-only transport the rest of the
// model commands use, and never fetches a tree or weights. By default it keeps only
// candidates whose weights are a safe format (safetensors or GGUF), since those are the
// ones bless can actually verify; pass --all to see every match including pickle-only
// repos.
func runModelSearch(args []string, _ string, out io.Writer) error {
	includeAll, args := takeFlag(args, "--all")
	args, author := takeValue(args, "--author")
	args, sort := takeValueOr(args, "--sort", "downloads")
	args, limitStr := takeValueOr(args, "--limit", "20")
	args, filters := takeValues(args, "--tag")

	// --gguf and --safetensors are convenience filters for the two safe weight formats.
	wantGGUF, args := takeFlag(args, "--gguf")
	wantSafetensors, args := takeFlag(args, "--safetensors")
	if wantGGUF {
		filters = append(filters, "gguf")
	}
	if wantSafetensors {
		filters = append(filters, "safetensors")
	}

	query := strings.TrimSpace(strings.Join(args, " "))
	if query == "" && author == "" && len(filters) == 0 {
		return errors.New("models search: a search term is required, for example `flynn models search qwen2.5 7b instruct`")
	}

	limit, err := strconv.Atoi(strings.TrimSpace(limitStr))
	if err != nil || limit <= 0 {
		return fmt.Errorf("models search: --limit must be a positive number, got %q", limitStr)
	}

	ctx := context.Background()
	hub := huggingface.New()
	results, err := hub.Search(ctx, huggingface.SearchQuery{
		Text:    query,
		Filters: filters,
		Author:  author,
		Sort:    sort,
		Limit:   limit,
	})
	if err != nil {
		return fmt.Errorf("models search: %w", err)
	}

	shown := 0
	var hidden int
	for _, r := range results {
		if !includeAll && !r.SafeFormat() {
			hidden++
			continue
		}
		printSearchResult(out, r)
		shown++
	}

	if shown == 0 {
		if hidden > 0 {
			_, _ = fmt.Fprintf(out, "no candidates with a safe weight format; %d match(es) are pickle-only. Re-run with --all to see them.\n", hidden)
			return nil
		}
		_, _ = fmt.Fprintln(out, "no models matched.")
		return nil
	}

	_, _ = fmt.Fprintf(out, "\nbless a candidate with `flynn models bless hf:<owner>/<name>`")
	if hidden > 0 {
		_, _ = fmt.Fprintf(out, " (%d pickle-only match(es) hidden; --all to show)", hidden)
	}
	_, _ = fmt.Fprintln(out)
	return nil
}

// takeValues removes every occurrence of a repeatable value flag from args and returns
// the collected values, so a flag like --tag can be passed more than once to AND several
// filters together.
func takeValues(args []string, name string) (rest []string, values []string) {
	out := args[:0:0]
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			values = append(values, args[i+1])
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out, values
}

// printSearchResult renders one candidate: the bless-ready reference on its own line,
// then the popularity and format signals indented beneath it.
func printSearchResult(out io.Writer, r huggingface.SearchResult) {
	format := "no safe format (pickle-only)"
	if r.SafeFormat() {
		format = safeFormatLabel(r)
	}
	pipeline := r.Pipeline
	if pipeline == "" {
		pipeline = "unspecified"
	}
	_, _ = fmt.Fprintf(out, "\nhf:%s\n", r.ID)
	_, _ = fmt.Fprintf(out, "  %s downloads, %s likes | %s | %s\n",
		humanCount(r.Downloads), humanCount(r.Likes), pipeline, format)
}

// safeFormatLabel names the safe weight format(s) a candidate advertises, for the
// common case where exactly one is present.
func safeFormatLabel(r huggingface.SearchResult) string {
	var hasST, hasGGUF bool
	for _, t := range r.Tags {
		switch strings.ToLower(t) {
		case "safetensors":
			hasST = true
		case "gguf":
			hasGGUF = true
		}
	}
	switch {
	case hasST && hasGGUF:
		return "safetensors + gguf"
	case hasGGUF:
		return "gguf"
	default:
		return "safetensors"
	}
}

// humanCount renders a count compactly (1.2k, 3.4M) so a long results list stays
// readable; small counts print in full.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}
