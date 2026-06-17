// Package mcp — MCP-facade Keeper-а (M0.7).
//
// Транспорт — Streamable HTTP по MCP spec (JSON-RPC 2.0 поверх HTTP+SSE).
// Listener: `listen.mcp.addr` (отдельный port от Operator API).
// Auth — JWT Bearer (тот же, что Operator API, общий verify через
// [jwt.Verifier] + RBAC через [rbac.Holder] / [rbac.Enforcer]).
//
// Каталог tools 1:1 с HTTP endpoints; источник правды по семантике —
// docs/keeper/mcp-tools.md и docs/keeper/operator-api.md. MCP-сторона
// не дублирует бизнес-логику: tool-handler-ы вызывают тот же
// [operator.Service] / [rbac.Service] / incarnation-слой, что HTTP-handler-ы
// (PM-decision M0.7 #6).
//
// Реализованы: 3 operator-tools (create / revoke / issue-token), 6 role-tools
// (RBAC-CRUD поверх [rbac.Service], Slice 2b), 7 incarnation-tools
// (create / run / get / list / history / unlock / upgrade), 2 soul-tools
// (create / issue-token — паритет REST POST /v1/souls + issue-token). Остаются
// stub (incarnation.destroy / soul.list / push / cloud) — объявлены в манифесте и при
// tools/call возвращают MCP-error `internal-error` с `data.code:
// "not-implemented"`. SSE / async / `_apply_id`-callback — M0.7.c.
//
// Содержимое пакета:
//
//   - jsonrpc.go — JSON-RPC 2.0 wire-формат и базовый dispatch.
//   - server.go — HTTP listener + lifecycle.
//   - manifest.go — каталог tool declarations (дин. из catalogManifest,
//     актуальное число под контролем TestCatalog_TotalCount).
//   - handler.go — tools/call dispatcher.
//   - errors.go — RFC 7807 → MCP-error mapper.
//   - middleware/ — JWT auth / RBAC / audit обёртки.
package mcp

import (
	"encoding/json"
)

// JSON-RPC 2.0 protocol-version поля. MCP-spec требует `"jsonrpc": "2.0"`
// в каждом message; никаких других значений не принимаем.
const jsonRPCVersion = "2.0"

// jsonRPCRequest — JSON-RPC 2.0 request-object.
//
// `id` опционален (notifications не имеют id); по MCP-spec у нас всегда
// request с id, поэтому в обработке мы требуем непустой id для
// regular-методов и допускаем omitted только в notifications (которые
// сервер сейчас не отдаёт обратно).
//
// `Params` — raw json.RawMessage, чтобы дальнейший дескриминатор по
// method-у мог распарсить под конкретную схему (`tools/call` params
// отличается от `initialize`).
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse — JSON-RPC 2.0 response-object. Поля Result и Error
// взаимоисключающие: либо Result (success), либо Error (failure).
// json.RawMessage у Result — чтобы инкапсулировать произвольный output
// без двойного marshal-а.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

// jsonRPCError — JSON-RPC 2.0 error-object. Code — стабильный целочисленный
// код (см. константы ниже); Message — короткое описание; Data —
// произвольный structured payload (для MCP мы кладём туда code/instance).
type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC 2.0 reserved error codes (spec § 5.1):
//
//   - -32700 Parse error      — invalid JSON.
//   - -32600 Invalid Request  — request-object не валиден.
//   - -32601 Method not found — method не зарегистрирован.
//   - -32602 Invalid params   — params не подходит под method-signature.
//   - -32603 Internal error   — внутренняя ошибка сервера.
//
// MCP-specific: ошибки tool-execution возвращаются как Internal Error
// (-32603) с MCP-payload в `data` (см. errors.go).
const (
	rpcCodeParseError     = -32700
	rpcCodeInvalidRequest = -32600
	rpcCodeMethodNotFound = -32601
	rpcCodeInvalidParams  = -32602
	rpcCodeInternalError  = -32603
)

// newRPCError — конструктор response-объекта с error. ID копируется из
// request-а (null если запрос не парсился).
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

// newRPCResult — конструктор success-response. result-payload уже
// сериализован (json.RawMessage) — caller marshal-ит result-struct сам.
func newRPCResult(id json.RawMessage, result json.RawMessage) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Result:  result,
	}
}
