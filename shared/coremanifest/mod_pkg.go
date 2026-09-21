package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modPkg declares core.pkg.
var modPkg = schema.Module{
	Name:         "pkg",
	Capabilities: []schema.Capability{schema.RunAsRoot, schema.ExecSubprocess},
	States: map[string]schema.State{
		"absent": {
			Description: "Package removed.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Package name."},
			},
		},
		"installed": {
			Description: "Package installed (optionally at an exact version). Idempotent (ADR-015).",
			Input: schema.Input{
				"name":    {Type: schema.String, Required: true, Description: "Package name."},
				"version": {Type: schema.String, Description: "Exact package version (any if omitted)."},
			},
		},
		"latest": {
			Description: "Package installed and updated to the latest available version.",
			Input: schema.Input{
				"name": {Type: schema.String, Required: true, Description: "Package name."},
			},
		},
	},
}
