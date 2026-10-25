//go:build integration

package ldapsync

import "github.com/souls-guild/soul-stack/keeper/internal/integrationenv"

// requireDocker — package-local shim over [integrationenv.RequireDocker].
func requireDocker() bool {
	return integrationenv.RequireDocker()
}
