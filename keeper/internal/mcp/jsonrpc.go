// Package mcp — Keeper's MCP facade (M0.7).
//
// Transport is Streamable HTTP per the MCP spec (JSON-RPC 2.0 over
// HTTP+SSE). Listener: `listen.mcp.addr` (separate port from the Operator
// API). Auth is JWT Bearer (same as the Operator API, shared verify via
// [jwt.Verifier] + RBAC via [rbac.Holder] / [rbac.Enforcer]).
//
// The tool catalog is 1:1 with HTTP endpoints; source of truth for
// semantics is docs/keeper/mcp-tools.md and docs/keeper/operator-api.md.
// The MCP side doesn't duplicate business logic: tool handlers call the
// same [operator.Service] / [rbac.Service] / incarnation layer as the HTTP
// handlers (PM-decision M0.7 #6).
//
// Shipped as of M0.7: 3 operator-tools (create / revoke / issue-token), 6
// role-tools (RBAC CRUD over [rbac.Service], Slice 2b), 7 incarnation-tools
// (create / run / get / list / history / unlock / upgrade), 2 soul-tools
// (create / issue-token — parity with REST POST /v1/souls + issue-token).
// Still stubbed: soul.list / push / cloud — declared in the manifest,
// return MCP-error `internal-error` with `data.code: "not-implemented"` on
// tools/call. SSE / async / `_apply_id` callback — M0.7.c.
//
// Package contents:
//
//   - jsonrpc.go — JSON-RPC 2.0 wire format and base dispatch.
//   - server.go — HTTP listener + lifecycle.
//   - manifest.go — tool-declaration catalog (built from catalogManifest;
//     kept accurate by TestCatalog_TotalCount).
//   - handler.go — tools/call dispatcher.
//   - errors.go — RFC 7807 → MCP-error mapper.
package mcp

import (
	"encoding/json"
)

// JSON-RPC 2.0 protocol-version field. The MCP spec requires
// `"jsonrpc": "2.0"` in every message; no other values are accepted.
const jsonRPCVersion = "2.0"

// jsonRPCRequest — JSON-RPC 2.0 request object.
//
// `id` is optional (notifications have none); per the MCP spec we always
// expect a request with id, so handling requires a non-empty id for
// regular methods and allows omission only for notifications (which the
// server doesn't currently send back).
//
// `Params` stays raw json.RawMessage so the method-based dispatcher can
// later parse it into the right schema (`tools/call` params differ from
// `initialize`).
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse — JSON-RPC 2.0 response object. Result and Error are
// mutually exclusive: either Result (success) or Error (failure). Result
// is json.RawMessage to hold arbitrary output without a double marshal.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError — JSON-RPC 2.0 error object. Code is a stable integer (see
// the constants below); Message is a short description; Data is an
// arbitrary structured payload (for MCP we put code/instance there).
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC 2.0 reserved error codes (spec § 5.1):
//
//   - -32700 Parse error      — invalid JSON.
//   - -32600 Invalid Request  — request object is not valid.
//   - -32601 Method not found — method is not registered.
//   - -32602 Invalid params   — params don't match the method signature.
//   - -32603 Internal error   — internal server error.
//
// MCP-specific: tool-execution errors come back as Internal Error (-32603)
// with an MCP payload in `data` (see errors.go).
const (
	rpcCodeParseError     = -32700
	rpcCodeInvalidRequest = -32600
	rpcCodeMethodNotFound = -32601
	rpcCodeInvalidParams  = -32602
	rpcCodeInternalError  = -32603
)

// newRPCError builds a response object carrying an error. ID is copied
// from the request (null if the request itself failed to parse).
func newRPCError(id json.RawMessage, code int, message string, data any) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Error: &jsonRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

// newRPCResult builds a success response. The result payload is already
// serialized (json.RawMessage) — the caller marshals the result struct
// itself.
func newRPCResult(id json.RawMessage, result json.RawMessage) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Result:  result,
	}
}
