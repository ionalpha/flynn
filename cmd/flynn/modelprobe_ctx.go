package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// servedContext asks a running llama.cpp server how large a context it was started with, in
// tokens, and returns 0 when it cannot say. The probe cannot read this from the launch
// configuration: a launch with no context size lets the runtime pick one to fit the device, which
// can be well below the model's advertised window, and only the server knows what it chose. The
// answer is a bound for the context ladder, not a requirement, so any failure to get it leaves
// the ladder to end at the server's own refusal instead.
func servedContext(ctx context.Context, httpc *http.Client, baseURL string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// /props is served at the root, beside the /v1 API the base URL points into.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")+"/props", http.NoBody)
	if err != nil {
		return 0
	}
	if httpc == nil {
		httpc = &http.Client{}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var props struct {
		Settings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&props); err != nil {
		return 0
	}
	return max(props.Settings.NCtx, 0)
}
