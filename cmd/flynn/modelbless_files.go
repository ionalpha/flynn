package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/huggingface"
)

// weightIndexName is the file a sharded safetensors repository uses to say which shard
// holds each tensor.
const weightIndexName = "model.safetensors.index.json"

// chatTemplateFile is the chat template a repository ships beside its tokenizer.
const chatTemplateFile = "chat_template.jinja"

// findFile returns the file at exactly path.
func findFile(files []huggingface.File, path string) (huggingface.File, bool) {
	for _, f := range files {
		if f.Path == path {
			return f, true
		}
	}
	return huggingface.File{}, false
}

// pruneToWeightIndex keeps the safetensors files the weight index names and drops any
// other, so a shard the repository lists but nothing loads (a leftover from an earlier
// upload) is not counted into the model's size or downloaded. It refuses an index that
// names a shard the repository does not have, since that model could not load.
func pruneToWeightIndex(files []huggingface.File, index []byte) ([]huggingface.File, error) {
	var raw struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(index, &raw); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", weightIndexName, err)
	}
	named := make(map[string]bool, len(raw.WeightMap))
	for _, shard := range raw.WeightMap {
		named[shard] = true
	}
	if len(named) == 0 {
		return nil, fmt.Errorf("%s names no weight files", weightIndexName)
	}
	listed := make(map[string]bool, len(files))
	kept := make([]huggingface.File, 0, len(files))
	for _, f := range files {
		listed[f.Path] = true
		if strings.EqualFold(filepath.Ext(f.Path), ".safetensors") && !named[f.Path] {
			continue
		}
		kept = append(kept, f)
	}
	for shard := range named {
		if !listed[shard] {
			return nil, fmt.Errorf("%s names %s, which the repository does not list", weightIndexName, shard)
		}
	}
	return kept, nil
}

// keptBytes names the small files whose content bless reads after hashing them.
var keptBytes = map[string]bool{
	"config.json":           true,
	weightIndexName:         true,
	"tokenizer_config.json": true,
	chatTemplateFile:        true,
}

type resolvedFile struct {
	qf   catalog.QuantFile
	data []byte
}

// smallFileResolver turns a tree entry into a verified [catalog.QuantFile]. A large LFS
// file already carries its content digest from the Hub; a small file is downloaded once
// through the verified transport and pinned to the sha256 of the bytes that arrived. A
// file asked for twice is fetched once.
type smallFileResolver struct {
	ctx      context.Context
	hub      *huggingface.Client
	dl       *fetch.Downloader
	repoPath string
	tmpDir   string
	out      io.Writer
	cache    map[string]resolvedFile
}

// resolve returns the file's manifest entry and, for the few small files bless reads, its
// content.
func (r *smallFileResolver) resolve(f huggingface.File) (catalog.QuantFile, []byte, error) {
	if c, ok := r.cache[f.Path]; ok {
		return c.qf, c.data, nil
	}
	url := r.hub.FileURL(r.repoPath, f.Path)
	qf := catalog.QuantFile{Name: f.Path, URL: url, SizeBytes: f.Size}
	var data []byte
	if f.SHA256 != "" {
		qf.Digest = "sha256:" + f.SHA256
	} else {
		_, _ = fmt.Fprintf(r.out, "hashing %s (small file, fetched to verify)\n", f.Path)
		res, err := r.dl.Fetch(r.ctx, fetch.Request{
			URL:      url,
			Dest:     filepath.Join(r.tmpDir, filepath.Base(f.Path)),
			MaxBytes: f.Size + (1 << 20),
		})
		if err != nil {
			return catalog.QuantFile{}, nil, fmt.Errorf("hash %s: %w", f.Path, err)
		}
		qf.Digest = "sha256:" + res.SHA256
		qf.SizeBytes = res.Bytes
		if keptBytes[f.Path] {
			data, _ = os.ReadFile(res.Path)
		}
	}
	if r.cache == nil {
		r.cache = make(map[string]resolvedFile)
	}
	r.cache[f.Path] = resolvedFile{qf: qf, data: data}
	return qf, data, nil
}

// data returns the content read for a small file, or nil if it was not fetched.
func (r *smallFileResolver) data(path string) []byte { return r.cache[path].data }

// templateSource returns the chat template text a repository declares, from its own
// template file or the tokenizer config, or "" if it declares none. The text is
// untrusted: callers search it for control tokens and never render it.
func (r *smallFileResolver) templateSource() string {
	if b := r.data(chatTemplateFile); len(b) > 0 {
		return string(b)
	}
	var raw struct {
		ChatTemplate json.RawMessage `json:"chat_template"`
	}
	if json.Unmarshal(r.data("tokenizer_config.json"), &raw) != nil || len(raw.ChatTemplate) == 0 {
		return ""
	}
	var single string
	if json.Unmarshal(raw.ChatTemplate, &single) == nil {
		return single
	}
	// Some tokenizers declare several named templates (default, tool_use).
	var named []struct {
		Template string `json:"template"`
	}
	if json.Unmarshal(raw.ChatTemplate, &named) != nil {
		return ""
	}
	parts := make([]string, 0, len(named))
	for _, n := range named {
		parts = append(parts, n.Template)
	}
	return strings.Join(parts, "\n")
}
