// Service domain bodies. Only the one type the incarnation domain reuses lives
// here so far - the rest of the service domain is still declared in
// keeper/internal/api, so its consumers still restate it by hand and the guard
// in single_source_test.go cannot yet see them.

package wire

// StateSchemaMigration — native step of the migration chain (element ServiceStateSchemaReply.
// migrations). from/to — int. Declared here rather than beside the rest of the service domain
// because the incarnation upgrade-paths reply reuses it (UpgradePathTarget.state_migrations).
type StateSchemaMigration struct {
	From int    `json:"from"`
	Path string `json:"path"`
	To   int    `json:"to"`
}
