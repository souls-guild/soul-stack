package herald

// HMAC-подпись тела webhook-доставки (ADR-052(a)/(e): secret_ref = signing-
// token канала из Vault). Если у Herald задан secret_ref — worker резолвит
// секрет из Vault и подписывает тело запроса, кладя подпись в заголовок
// X-SoulStack-Signature. Приёмник верифицирует тем же общим секретом.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SignatureHeader — имя HTTP-заголовка с HMAC-подписью тела webhook-доставки.
// Формат значения — `sha256=<hex>` (parity GitHub `X-Hub-Signature-256` /
// общепринятая форма webhook-подписи: приёмник сразу видит алгоритм). Имя в
// нашем namespace `X-SoulStack-*`.
//
// NB(docs-writer): новое публичное имя заголовка (контракт webhook-приёмника) —
// зафиксировать в naming-rules при S4 (рядом с Herald/secret_ref).
const SignatureHeader = "X-SoulStack-Signature"

// signBody вычисляет HMAC-SHA256 тела body ключом secret и возвращает значение
// заголовка в форме `sha256=<hex>`. secret — резолвленный из Vault signing-token
// канала (raw-байты ключа).
func signBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
