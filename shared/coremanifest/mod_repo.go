package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modRepo declares core.repo.
var modRepo = schema.Module{
	Name:         "repo",
	Capabilities: []schema.Capability{schema.FSWriteRoot},
	States: map[string]schema.State{
		"absent": {
			Description: "Repository description removed (GPG key is not touched).",
			Input: schema.Input{
				"dest": {Type: schema.String, Description: "Absolute path of the repo description file to remove, if it was created with a custom dest (apt/dnf/yum)."},
				"name": {Type: schema.String, Required: true, Description: "Repository name (= description file name)."},
			},
		},
		"present": {
			Description: "Package repository declared (description file + GPG key); apt/dnf/yum/apk. ADR-015/ADR-016.",
			Input: schema.Input{
				"arch":         {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "apt repo architectures (e.g. [amd64]) -> arch= in options; apt only, [a-z0-9]."},
				"components":   {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "apt components (e.g. [main, contrib])."},
				"dest":         {Type: schema.String, Description: "Absolute path of the repo description file, overriding the backend default (apt /etc/apt/sources.list.d/<name>.list; dnf/yum /etc/yum.repos.d/<name>.repo). Not for apk (single repositories file)."},
				"enabled":      {Type: schema.Bool, Description: "Repository is enabled (default true)."},
				"gpg_check":    {Type: schema.Bool, Description: "Verify package signatures (default true; opt-out produces a warning)."},
				"gpg_key":      {Type: schema.String, Description: "GPG key (supply-chain): apt - inline keyring, materialized under /etc/apt/keyrings/ as <name>.asc when armored and <name>.gpg otherwise, since apt picks its parser from the extension; dnf/yum - URL or path in gpgkey=. Key-by-URL for apt - via core.url.fetched (ADR-071 option B)."},
				"gpg_key_path": {Type: schema.String, Description: "Path to a GPG key already present on the host: apt -> signed-by=<path>, dnf/yum -> gpgkey=<path>. The module only references it (no copy) and guards its existence on plan/apply. Mutually exclusive with gpg_key; deliver the key first (core.url.fetched/core.file). Not for apk."},
				"name":         {Type: schema.String, Required: true, Description: "Repository name (= description file name, [A-Za-z0-9._-])."},
				"suite":        {Type: schema.String, Description: "apt suite/distribution (e.g. stable)."},
				"uri":          {Type: schema.String, Required: true, Description: "Repository URL (http/https; http produces a mandatory warning)."},
			},
		},
	},
}
