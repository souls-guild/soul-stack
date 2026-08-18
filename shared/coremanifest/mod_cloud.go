package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// The driver of a cloud step comes from one of two sources, never both
// (NIM-668): `provider` names an entry of the providers registry, or
// `driver` + `credentials` carry the same tuple in the step itself. The
// exclusivity is a relation between params, which this DSL cannot state — it is
// enforced where it runs, in the module (keeper/internal/coremod/cloud/source.go).
// That is why neither is `required` here: requiring `provider` would reject an
// inline step before the module ever sees it. The cost is real and worth stating:
// `required` was the one gate that ran OFFLINE, so a step naming no source at all
// is now caught at run time, after earlier tasks of the same run have already
// touched hosts, instead of by soul-lint before the commit.
//
// `credentials` carries `pattern: "^vault:.*"` because the SDK demands exactly
// that of any `secret: true` param (sdk/schema/validate.go — a secret without it
// is a manifest error). It is checked against the DECLARATION, never against an
// authored value: nothing reads Param.Pattern while linting a step. The refusal
// an author actually meets is the module's own (source.go).
const (
	descProvider    = "Name of an entry of the providers registry (driver + credentials + region). Mutually exclusive with driver/credentials."
	descDriver      = "CloudDriver plugin alias to call, e.g. `wb` (soul-cloud-wb). Carries the driver in the step instead of the providers registry; requires credentials."
	descCredentials = "Vault reference keeper reads and hands to the driver, e.g. `vault:secret/cloud/wb-dev`. A reference, never a value. Requires driver."
	descRegion      = "Region passed to the driver alongside the credentials. Inline source only - with `provider` the registry entry already holds it."

	// `profile` is a typed dispatch the DSL cannot state: a STRING names an entry of
	// the profiles registry, an OBJECT is the VM spec itself (NIM-668). ParamType is
	// a closed set with no union and every param must declare one (the SDK validator
	// raises input_type_missing otherwise), so the declaration names the object form
	// and the module reads the actual type (resolveProfileParam). Consequence worth
	// knowing: a LITERAL `profile: some-row-name` is rejected statically by soul-lint
	// as param_type_mismatch - the registry form has to reach the param through an
	// expression (`${ vars.profile }`), which is how every service in the tree writes
	// it and how it worked before NIM-668 too.
	descProfile = "VM spec handed to the driver as-is. A string instead names an entry of the profiles registry (independent of the providers one - allowed with an inline driver)."
)

// modCloud declares core.cloud.
var modCloud = schema.Module{
	Name: "cloud",
	States: map[string]schema.State{
		"created": {
			Description: "Keeper-side (on:keeper). Cloud VMs created through a CloudDriver plugin (ADR-017).",
			Input: schema.Input{
				"count":             {Type: schema.Int, Description: "How many VMs to create (default 1, >= 1)."},
				"credentials":       {Type: schema.String, Secret: true, Pattern: "^vault:.*", Description: descCredentials},
				"driver":            {Type: schema.String, Description: descDriver},
				"fqdn_suffix":       {Type: schema.String, Description: "Suffix keeper predicts the self-onboard FQDN from: <name>-<index>.<suffix>. Inline source only - with `provider` the registry entry already holds it."},
				"generate_userdata": {Type: schema.Bool, Description: "Generate userdata from keeper.yml cloud_init (ADR-017(h))."},
				"name":              {Type: schema.String, Description: "Base name for the VM batch (self-onboard Variant T): keeper predicts FQDN=<name>-<index>.<suffix>. Required with self_onboard."},
				"profile":           {Type: schema.Map, Description: descProfile},
				"provider":          {Type: schema.String, Description: descProvider},
				"region":            {Type: schema.String, Description: descRegion},
				"self_onboard":      {Type: schema.Bool, Description: "The VM onboards itself from cloud-init (Variant T, ADR-017(h)): keeper predicts the FQDN and bakes per-VM tokens into userdata. Requires name + a suffix (fqdn_suffix inline, providers.fqdn_suffix in the registry)."},
				"userdata":          {Type: schema.String, Description: "Cloud-init userdata (mutually exclusive with generate_userdata and self_onboard)."},
			},
		},
		"destroyed": {
			Description: "Keeper-side (on:keeper). Cloud VMs destroyed, registries cascade-updated (ADR-017).",
			Input: schema.Input{
				"credentials": {Type: schema.String, Secret: true, Pattern: "^vault:.*", Description: descCredentials},
				"driver":      {Type: schema.String, Description: descDriver},
				"provider":    {Type: schema.String, Description: descProvider},
				"region":      {Type: schema.String, Description: descRegion},
				"sids":        {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "SIDs for cascade-updating registries (souls/seeds/tokens)."},
				"vm_ids":      {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "List of provider-vm-ids to destroy."},
			},
		},
		"resized": {
			Description: "Keeper-side (on:keeper). Cloud VM resources changed to an absolute target size via CloudDriver.Resize (capability Resizable).",
			Input: schema.Input{
				"allow_downtime": {Type: schema.Bool, Description: "Allow downtime (stop/start) for cpu/ram resize. Required when changing cpu/ram; disk-only is online (default false)."},
				"credentials":    {Type: schema.String, Secret: true, Pattern: "^vault:.*", Description: descCredentials},
				"desired":        {Type: schema.Map, Required: true, Description: "Target resources in our units: cpu_cores (cores) / ram_mb (MB) / disk_gb (GB). At least one > 0; fields with 0 are unchanged."},
				"driver":         {Type: schema.String, Description: descDriver},
				"provider":       {Type: schema.String, Description: descProvider},
				"region":         {Type: schema.String, Description: descRegion},
				"vm_ids":         {Type: schema.List, Required: true, Items: &schema.Param{Type: schema.String}, Description: "provider-vm-ids to resize (one target for the whole batch)."},
			},
		},
	},
}
