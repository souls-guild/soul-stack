package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// modSoul declares core.soul.
var modSoul = schema.Module{
	Name: "soul",
	States: map[string]schema.State{
		"registered": {
			Description: "Keeper-side (on:keeper). The bind act — a Soul with sid becomes a member of the run's incarnation (incarnation_membership) and optionally carries stable Coven tags (ADR-017; ADR-008 amendment NIM-124). Onboarding barrier — ADR-061.",
			Input: schema.Input{
				"await_min_count":     {Type: schema.Int, Description: "Minimum number of online hosts for the barrier to succeed. Default — the number of SIDs being registered (all)."},
				"await_online":        {Type: schema.Bool, Description: "Onboarding barrier (ADR-061): after registration, block waiting until the created Souls become online (Redis SID lease)."},
				"await_poll_interval": {Type: schema.String, Format: "duration", Description: "Presence polling period (default ~2s)."},
				"await_timeout":       {Type: schema.String, Format: "duration", Description: "Upper bound on the barrier wait. REQUIRED when await_online: true. Ceiling — keeper.yml::max_await_timeout."},
				"coven":               {Type: schema.List, Items: &schema.Param{Type: schema.String}, Description: "Optional set of stable Coven tags (kebab-case), applied to all SIDs. Membership is implicit (incarnation_membership), NOT a coven value — ADR-008 amendment NIM-124."},
				"mode":                {Type: schema.String, Description: "append (default) | replace | remove."},
				"refresh_soulprint":   {Type: schema.Bool, Description: "Mid-run re-resolve of the run's roster (ADR-061; stratification/re-resolve — slices S2/S3, echo refreshed:false)."},
				// sid — a string OR a list of strings (ADR-061). Declared as string for backward
				// compatibility (a single literal string remains a valid author form). In practice
				// a list of SIDs arrives via a CEL expression `${ register.<step>.hosts }` (e.g.
				// hosts produced by an earlier keeper step): ${…} values are skipped by soul-lint's
				// type-check regardless of type. The stripped-down schema DSL does not express the
				// union string|list; the runtime (StringOrSliceParam) accepts both forms, and the
				// await barrier aggregates over the whole set. A literal list `sid: [a,b]`
				// accordingly does NOT pass soul-lint's static type-check (it arrives via CEL) — a
				// deliberate trade-off, see ADR-061.
				"sid": {Type: schema.String, Required: true, Description: "SID (FQDN) of the target Soul: a single string OR a list (via CEL `${ register.<step>.hosts }`) — registering N hosts in one barrier step, ADR-061."},
			},
		},
	},
}
