package audit

// PayloadActorJWTID is the payload key naming the token an audited action was
// taken with: the `jti` of the caller's JWT, which is its `operator_jwts` row
// (ADR-014 amendment NIM-1045). With it an action can be traced to the one
// token behind it — and to where that token was minted and last used from —
// rather than only to the operator, who may hold several.
const PayloadActorJWTID = "actor_jwt_id"

// WithActorJWTID returns payload with [PayloadActorJWTID] set to jwtID. An
// empty jwtID (a token minted before per-token accounting, or no token at all)
// leaves payload as it is; a nil payload gets a map only when there is
// something to put in it.
func WithActorJWTID(payload map[string]any, jwtID string) map[string]any {
	if jwtID == "" {
		return payload
	}
	if payload == nil {
		payload = make(map[string]any, 1)
	}
	payload[PayloadActorJWTID] = jwtID
	return payload
}
