package herald

// HMAC signature of webhook delivery body (ADR-052(a)/(e): secret_ref = signing-
// token of channel from Vault). If Herald has secret_ref — worker resolves
// secret from Vault and signs the request body, putting signature in the
// X-SoulStack-Signature header. Receiver verifies with the same shared secret.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// SignatureHeader is the name of the HTTP header with HMAC signature of webhook delivery body.
// Value format is `sha256=<hex>` (parity with GitHub `X-Hub-Signature-256` /
// standard webhook-signature form: receiver immediately sees the algorithm). Name in
// our namespace `X-SoulStack-*`.
//
// NB(docs-writer): new public header name (webhook receiver contract) —
// fix in naming-rules at S4 (next to Herald/secret_ref).
const SignatureHeader = "X-SoulStack-Signature"

// signBody calculates HMAC-SHA256 of body with secret key and returns header value
// in form `sha256=<hex>`. secret is the resolved signing-token from Vault
// (raw key bytes).
func signBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
