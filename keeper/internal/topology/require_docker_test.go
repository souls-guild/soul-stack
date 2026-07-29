//go:build integration

package topology

import "github.com/souls-guild/soul-stack/keeper/internal/integrationenv"

// requireDocker — package-local shim over the single policy decision
// ([integrationenv.RequireDocker]): a container-setup failure fails this suite
// unless a skip was asked for out loud. Kept per package so the call sites read
// the same as before; the decision itself lives in one place (NIM-238).
func requireDocker() bool {
	return integrationenv.RequireDocker()
}
