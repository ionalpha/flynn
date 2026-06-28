package extension

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/mission"
)

// recordHandler is a test Point that records every OnLoad and OnUnload and
// can be told to fail a load or an unload. It optionally contributes tools, so the
// same fixture exercises both the loader and the tool-bridge.
type recordHandler struct {
	capability string

	mu        sync.Mutex
	loads     []Mount  // every Mount passed to OnLoad, in order
	unloads   []string // every id passed to OnUnload, in order
	loadErr   error    // when set, OnLoad fails
	unloadErr error    // when set, OnUnload fails

	// toolsByID, when non-nil, makes the handler a ToolSource returning these tools
	// for the matching extension id.
	toolsByID map[string][]mission.Tool
}

func (h *recordHandler) Capability() string { return h.capability }

func (h *recordHandler) OnLoad(_ context.Context, m Mount) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.loadErr != nil {
		return h.loadErr
	}
	h.loads = append(h.loads, m)
	return nil
}

func (h *recordHandler) OnUnload(_ context.Context, id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unloads = append(h.unloads, id)
	return h.unloadErr
}

func (h *recordHandler) loadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.loads)
}

func (h *recordHandler) unloadCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.unloads)
}

// toolHandler is a recordHandler that also implements ToolSource.
type toolHandler struct {
	*recordHandler
}

func (h toolHandler) Tools(id string) []mission.Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toolsByID[id]
}

// fakeTool is a minimal mission.Tool with a stable name.
type fakeTool struct{ name string }

func (t fakeTool) Def() llm.Tool {
	return llm.Tool{Name: t.name, Description: "fake", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t fakeTool) Invoke(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok:" + t.name, nil
}

// errLoad is a sentinel a handler returns to fail a load.
var errLoad = errors.New("surface refused")
