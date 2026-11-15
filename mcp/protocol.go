package mcp

import (
	"bytes"
	"encoding/json"
)

// protocolVersion is the MCP revision implemented. The server echoes a
// client's non-empty requested version and answers with this one otherwise.
const protocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes. A governance denial is a tool result with isError
// set and never uses these.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// inbound keeps Params raw so each handler decodes only its own shape.
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (in inbound) isNotification() bool { return len(in.ID) == 0 }

// response sets exactly one of Result or Error.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// frame writes the request id byte for byte. encoding/json would compact and
// HTML-escape an embedded RawMessage, turning "&" in a string id into &.
// An empty ID, from a parse error, is written as null per JSON-RPC 2.0.
func (r response) frame() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	if len(r.ID) == 0 {
		b.WriteString("null")
	} else {
		b.Write(r.ID)
	}
	if r.Error != nil {
		e, err := json.Marshal(r.Error)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"error":`)
		b.Write(e)
	} else {
		res, err := json.Marshal(r.Result)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"result":`)
		b.Write(res)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Info is the name and version a party reports in the initialize handshake.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams ignores the client's capabilities, because the server offers
// only tools.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      Info               `json:"serverInfo"`
}

// serverCapabilities omits resources, prompts and sampling so a client does not
// probe for them.
type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

// toolsCapability sends ListChanged false: the toolset is fixed for a session.
type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

// toolDef's fields match both the MCP schema and llm.Tool, so a tool's Def
// converts directly.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// callParams passes Arguments to the tool verbatim.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// callResult.IsError marks a tool-level failure (a bad argument, a denied
// capability, a failed command) the model can adapt to. A protocol error means
// a malformed request.
type callResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is produced only as text, because a tool returns a string.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textContent(s string) []contentBlock {
	return []contentBlock{{Type: "text", Text: s}}
}

// emptyObjectSchema stands in for a missing schema, because MCP requires
// inputSchema to be an object.
var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)
