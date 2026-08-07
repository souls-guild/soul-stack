// Command bundle-artifact is a real module bundle, built by the soul-mod tests so the
// stamp/verify path runs against an actual executable rather than a stand-in.
//
// Its schema is deliberately steerable: SOUL_MOD_TEST_DESCRIPTION changes what the
// `acl` module says about itself, which is how a test makes the code disagree with an
// already-stamped artifact without rebuilding anything.
package main

import (
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
)

type acl struct{ module.BaseModule }

func (a *acl) Apply(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return nil
}

type info struct{ module.BaseModule }

func (i *info) Apply(*pluginv1.ApplyRequest, grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	return nil
}

func main() {
	description := os.Getenv("SOUL_MOD_TEST_DESCRIPTION")
	if description == "" {
		description = "Redis ACL users"
	}
	module.ServeBundle(module.Bundle{
		Compat: module.Compat{Keeper: ">=0.9 <2.0"},
		Modules: []module.Def{
			{
				Name:         "acl",
				Description:  description,
				Capabilities: []module.Capability{module.NetworkOutbound},
				SideEffects:  []module.SideEffect{{User: "redis_acl_user"}},
				Impl:         &acl{},
				States: map[string]module.State{
					"present": {
						Description: "The ACL user exists, enabled as requested",
						Input: module.Input{
							"host": {Type: module.String, Required: true, Description: "Redis host to connect to"},
							"port": {Type: module.Int, Default: 6379},
						},
					},
					"absent": {Description: "The ACL user is gone", Input: module.Input{
						"host": {Type: module.String, Required: true},
					}},
				},
			},
			{
				Name:        "info",
				Description: "Read-only Redis facts",
				Impl:        &info{},
				States: map[string]module.State{
					"reported": {Description: "Facts are reported"},
				},
			},
		},
	})
}
