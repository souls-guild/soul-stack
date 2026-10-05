package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modSSH declares the keeper-side agentless transport (NIM-849, ADR-063
// amendment 2026-09-12): `run` executes shell steps, `apply` applies a destiny
// (NIM-905).
//
// It is the ONLY core module that can execute anything on a host with no agent
// on it: `core.exec.run` and `core.file.present` are Soul-side, and a freshly
// created VM has no Soul yet. What the commands do is the site's own policy and
// is deliberately not expressible here — the module carries a transport and a
// secret floor, never an opinion about what gets installed.
var modSSH = schema.Module{
	Name: "ssh",
	States: map[string]schema.State{
		"run": {
			Description: "Keeper-side (on:keeper). Open an SSH session to each host and execute an ordered list of shell steps on it, feeding a value to a step's stdin (NIM-849).",
			Input: schema.Input{
				"hosts":             {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.Map}, Description: "Per-host objects, in practice `${ register.<mint>.hosts }`. `sid` is required; `primary_ip` only for the direct transport; an entry with `onboarded: true` is skipped, not dialed."},
				"join_wait_timeout": {Type: schema.String, Format: "duration", Description: "Ceiling on the wait for a host to become reachable, default 15m — a Teleport join, or an sshd that has not started yet on a freshly created VM. Bounds the connect on both transports; a refused host key (either transport), a rejected handshake on direct, an Authorize deny and a non-zero exit are not waits and fail at once. Integer seconds are also accepted at runtime."},
				"ssh_port":          {Type: schema.Int, Description: "SSH port (default 22)."},
				"ssh_provider":      {Type: schema.String, Required: true, Description: "SshProvider name for Authorize/Sign; retained as audit metadata in Teleport mode."},
				"ssh_user":          {Type: schema.String, Description: "SSH user (default root)."},
				// The element shape (`run`/`stdin`/`stdin_from`) is not expressible in
				// this schema DSL — Items carries a type, not nested properties — so it
				// is described here and checked in the module. The description is the
				// author-facing contract either way.
				"steps": {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.Map}, Description: "Ordered steps, each `run:` (a shell command line) plus at most one stdin source: `stdin:` (a literal, the same on every host) or `stdin_from:` (the name of a host field). A `run:` holding a declared secret is REFUSED — argv is visible in ps, audit.log and journald on the host itself."},
			},
			Output: schema.Output{
				"count":   {Type: schema.Int, Description: "Hosts in the list."},
				"hosts":   {Type: schema.List, Items: &schema.Param{Type: schema.Map}, Description: "Per-host outcome `{sid, ran, skipped}`. No command output: stdout is not captured."},
				"skipped": {Type: schema.Int, Description: "Hosts that carried `onboarded: true` and were not dialed."},
			},
		},
		"apply": {
			Description: "Keeper-side. Apply a destiny to each host of a list over SSH: deliver the agent binary and run `soul apply` with the destiny the Keeper rendered for that host. The list, not the registry, says who the hosts are; no `souls` row is read or written (NIM-905).",
			Input: schema.Input{
				"destiny":           {Type: schema.String, Required: true, Description: "A destiny declared in the service's `service.yml destiny[]`, resolved at its ref as `apply: destiny:` resolves it."},
				"hosts":             {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.Map}, Description: "Per-host objects, in practice `${ register.<mint>.hosts }`. `sid` is required and unique (hosts run at once); `primary_ip` only for the direct transport; an entry with `onboarded: true` is skipped, not dialed."},
				"input":             {Type: schema.Map, Description: "The destiny's input, the same for every host."},
				"input_from":        {Type: schema.Map, Items: &schema.Param{Type: schema.String}, Description: "Per-host input: destiny input name → the name of a top-level string field of the host entry (e.g. `bootstrap_token: bootstrap_token`). A host that will be dialed and lacks the field, or carries it empty, is refused before any host is dialed (an `onboarded: true` entry is not read); a name also set in `input` is refused."},
				"join_wait_timeout": {Type: schema.String, Format: "duration", Description: "Ceiling on the wait for a host to become reachable, default 15m — as for `core.ssh.run`. A rejected host key is never waited on."},
				"ssh_port":          {Type: schema.Int, Description: "SSH port (default 22)."},
				"ssh_provider":      {Type: schema.String, Required: true, Description: "SshProvider name for Authorize/Sign; retained as audit metadata in Teleport mode."},
				"ssh_user":          {Type: schema.String, Description: "SSH user (default root)."},
			},
			Output: schema.Output{
				"changed": {Type: schema.Int, Description: "Hosts where some task of the destiny changed something."},
				"count":   {Type: schema.Int, Description: "Hosts in the list."},
				"hosts":   {Type: schema.List, Items: &schema.Param{Type: schema.Map}, Description: "Per-host outcome `{sid, ran, skipped, changed}`. Nothing the destiny printed."},
				"skipped": {Type: schema.Int, Description: "Hosts that carried `onboarded: true` and were not dialed."},
			},
		},
	},
}
