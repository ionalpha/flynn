package mcp

import "encoding/json"

// protocolVersion is the Model Context Protocol revision this server implements.
// On initialize the server echoes the client's requested version when it is
// non-empty (the client and server negotiate the highest both support), falling
// back to this constant so a client that sends nothing still gets a valid answer.
const protocolVersion = "2025-06-18"

// JSON-RPC 2.0 error codes used on the wire. The reserved range is the standard
// set; a governance denial is not a protocol error (it is a normal tool result
// with isError set), so it never uses these.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// inbound is one JSON-RPC message received from the client. A request carries an
// ID and expects a response; a notification omits the ID and expects none. Params
// stays raw so each handler decodes only the shape it needs.
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message is a notification (no ID), which the
// server handles without writing a response, per JSON-RPC 2.0.
func (in inbound) isNotification() bool { return len(in.ID) == 0 }

// response is one JSON-RPC reply. Exactly one of Result or Error is set: a success
// carries Result and omits Error, a failure carries Error and omits Result. The ID
// echoes the request's ID verbatim so the client can pair reply to call.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object: a numeric code and a human message, with
// optional structured data.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Info identifies a party in the initialize handshake: the server's name and
// version reported to the client, and the shape the client reports back.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// initializeParams is the subset of the client's initialize request the server
// reads: the protocol version to negotiate. The client's capabilities and info are
// accepted and ignored, since this server offers only tools.
type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// initializeResult is the server's half of the handshake: the negotiated protocol
// version, the capabilities it offers (tools only), and its identity.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      Info               `json:"serverInfo"`
}

// serverCapabilities advertises what the server supports. Only tools are offered;
// resources, prompts, and sampling are absent so a client does not probe for them.
type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

// toolsCapability declares the tools feature. ListChanged is false: the exposed
// toolset is fixed for a session, so the server never emits a list-changed
// notification and a client need not subscribe.
type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// toolsListResult is the reply to tools/list: every tool the server exposes, each
// with the declaration the client hands to its model.
type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

// toolDef is one tool's declaration on the wire: its name, the description the
// model uses to decide when to call it, and the JSON Schema for its arguments. The
// field names match both the MCP schema and llm.Tool, so a Flynn tool's Def maps
// across directly.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// callParams is the tools/call request: which tool to run and the arguments to run
// it with. Arguments stays raw and is passed to the tool verbatim.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// callResult is the tools/call reply. Content carries the tool's output as blocks;
// IsError marks a tool-level failure (a bad argument, a denied capability, a failed
// command) so the calling model sees the failure and adapts, rather than the call
// being a protocol error. A protocol error is reserved for a malformed request.
type callResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// contentBlock is one piece of a tool result. Only text is produced here: a Flynn
// tool returns a string, which becomes a single text block.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// textContent wraps a string as the single-block content of a tool result.
func textContent(s string) []contentBlock {
	return []contentBlock{{Type: "text", Text: s}}
}

// emptyObjectSchema is the input schema substituted for a tool that declares none,
// since MCP requires inputSchema to be an object rather than null.
var emptyObjectSchema = json.RawMessage(`{"type":"object"}`)
