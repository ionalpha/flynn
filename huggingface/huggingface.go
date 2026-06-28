// Package huggingface is a read-only client for the Hugging Face Hub HTTP API: it
// lists the files in a model repository and reads a model's card metadata over the
// same hardened, public-only transport the file downloader uses. It is the upstream
// half of turning a hub reference into a verified catalog entry, and the foundation a
// discovery surface (search, list, view) is built on.
//
// It reads metadata only. It never downloads weights or executes anything: a file's
// bytes are fetched and verified separately through the download path. The value it
// adds is trust-bearing structure: for a large weights file the Hub records an LFS
// object id that is the file's sha256, so a manifest listed here already carries the
// content digest a download can be pinned to, captured from the registry rather than
// typed by hand.
package huggingface

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/netguard"
)

// defaultBase is the public Hub origin. It is configurable only so a test can point
// the client at a local server; production always talks to the real Hub over https.
const defaultBase = "https://huggingface.co"

// maxBodyBytes caps a metadata response so a hostile or runaway endpoint cannot
// exhaust memory. A repo tree or model card is kilobytes to low megabytes; this leaves
// generous room while staying bounded.
const maxBodyBytes = 16 << 20 // 16 MiB

// Client reads model metadata from the Hugging Face Hub over a hardened transport.
type Client struct {
	http *http.Client
	base string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient injects the HTTP client, so a test can supply one that reaches a
// local server. Production uses the default anti-SSRF, https-only transport.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		if c != nil {
			cl.http = c
		}
	}
}

// WithBaseURL overrides the Hub origin, for tests only.
func WithBaseURL(base string) Option {
	return func(cl *Client) {
		if base != "" {
			cl.base = strings.TrimRight(base, "/")
		}
	}
}

// New builds a Client. With nothing injected it talks to the public Hub over the
// public-only transport, which refuses any private, loopback, or metadata address.
func New(opts ...Option) *Client {
	c := &Client{base: defaultBase}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		c.http = netguard.Client(netguard.PublicOnly())
	}
	return c
}

// File is one file in a model repository's tree.
type File struct {
	// Path is the file's path within the repository, for example "model.safetensors"
	// or "tokenizer.json".
	Path string
	// Size is the file's size in bytes.
	Size int64
	// SHA256 is the content hash, present only for an LFS-tracked file (the large
	// weights), taken from the Hub's recorded LFS object id. It is empty for a small
	// git-tracked file, whose Hub object id is a git blob hash, not a content sha256;
	// such a file's digest is established by hashing it on download instead.
	SHA256 string
	// LFS reports whether the file is stored in LFS (the large binary objects). An LFS
	// file carries a usable SHA256; a non-LFS file does not.
	LFS bool
}

// treeEntry mirrors one element of the Hub's repo-tree response.
type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// Tree lists the files at the main revision of a model repository, with the content
// digest already attached to every LFS-tracked file. repo is "owner/name". A missing
// repo is reported as a terminal not-found rather than an empty list, so a typo never
// looks like an empty model.
func (c *Client) Tree(ctx context.Context, repo string) ([]File, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", c.base, repo)
	var entries []treeEntry
	if err := c.getJSON(ctx, endpoint, &entries); err != nil {
		return nil, err
	}
	files := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.Type != "file" {
			continue
		}
		f := File{Path: e.Path, Size: e.Size}
		if e.LFS != nil && e.LFS.OID != "" {
			f.SHA256 = e.LFS.OID
			f.LFS = true
			if e.LFS.Size > 0 {
				f.Size = e.LFS.Size
			}
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fault.New(fault.Terminal, "huggingface_empty_tree",
			"huggingface: model "+repo+" has no files (does it exist?)")
	}
	return files, nil
}

// Info is the subset of a model's card metadata used to describe a catalog entry.
type Info struct {
	// ID is the canonical "owner/name" identifier the Hub returns.
	ID string
	// Author is the publishing namespace.
	Author string
	// License is the SPDX-style license id from the model card, when declared.
	License string
	// Tags are the model-card tags, which carry signals like the pipeline type and the
	// base library.
	Tags []string
	// Gated reports whether the repo requires accepting terms before download, so a
	// caller can warn that an automated fetch will not succeed unattended.
	Gated bool
}

// infoResponse mirrors the Hub's model-info response.
type infoResponse struct {
	ID       string   `json:"id"`
	Author   string   `json:"author"`
	Tags     []string `json:"tags"`
	Gated    any      `json:"gated"` // false, or "auto"/"manual" when gated
	CardData struct {
		License string `json:"license"`
	} `json:"cardData"`
}

// Info reads a model's card metadata. repo is "owner/name".
func (c *Client) Info(ctx context.Context, repo string) (Info, error) {
	if err := checkRepo(repo); err != nil {
		return Info{}, err
	}
	endpoint := fmt.Sprintf("%s/api/models/%s", c.base, repo)
	var r infoResponse
	if err := c.getJSON(ctx, endpoint, &r); err != nil {
		return Info{}, err
	}
	info := Info{ID: r.ID, Author: r.Author, License: r.CardData.License, Tags: r.Tags}
	switch g := r.Gated.(type) {
	case bool:
		info.Gated = g
	case string:
		info.Gated = g != "" && g != "false"
	}
	return info, nil
}

// FileURL is the direct https location a file is downloaded and verified from. It
// resolves the main revision, the same revision Tree lists, so a digest from Tree
// pins the bytes this URL returns.
func (c *Client) FileURL(repo, path string) string {
	return fmt.Sprintf("%s/%s/resolve/main/%s", c.base, repo, path)
}

// getJSON performs a bounded GET and decodes a JSON body, mapping transport and status
// failures onto faults so a caller classifies them uniformly.
func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fault.Wrap(fault.Terminal, "huggingface_request", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fault.Wrap(fault.Transient, "huggingface_http", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fault.New(fault.Terminal, "huggingface_not_found",
			"huggingface: not found (HTTP 404) at "+endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		return fault.New(fault.Transient, "huggingface_status",
			fmt.Sprintf("huggingface: HTTP %d from %s", resp.StatusCode, endpoint))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fault.Wrap(fault.Transient, "huggingface_read", err)
	}
	if int64(len(body)) > maxBodyBytes {
		return fault.New(fault.Terminal, "huggingface_too_large",
			fmt.Sprintf("huggingface: metadata response exceeds the %d-byte cap", maxBodyBytes))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fault.Wrap(fault.Terminal, "huggingface_decode", err)
	}
	return nil
}

// checkRepo rejects an empty or malformed "owner/name" before a request is built, so a
// bad reference fails locally with a clear message rather than as a remote 404.
func checkRepo(repo string) error {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return fault.New(fault.Terminal, "huggingface_repo_empty", "huggingface: empty model reference")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fault.New(fault.Terminal, "huggingface_repo_form",
			"huggingface: model reference must be owner/name, got "+repo)
	}
	// A path segment must not smuggle traversal or a query into the request URL.
	if strings.ContainsAny(repo, "?#") || strings.Contains(repo, "..") {
		return fault.New(fault.Terminal, "huggingface_repo_chars",
			"huggingface: model reference has invalid characters: "+repo)
	}
	if _, err := url.Parse(defaultBase + "/api/models/" + repo); err != nil {
		return fault.New(fault.Terminal, "huggingface_repo_parse", "huggingface: unparseable model reference: "+repo)
	}
	return nil
}
