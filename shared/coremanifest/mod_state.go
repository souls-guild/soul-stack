package coremanifest

import "github.com/souls-guild/soul-stack/sdk/schema"

// paramStateField is the `field:` param, identical across every state — the write
// is always addressed by one top-level property of the service state_schema.
var paramStateField = schema.Param{
	Type: schema.String, Required: true,
	Description: "Top-level property name of the service state_schema to write. The Vault path of each declared secret is DERIVED from (service, incarnation, this field, the element's key) and is never authored.",
}

// stateValueParam builds the `value:` param. It is declared `string` because the
// author form is a CEL expression in every corpus case, and a CEL-wrapped value is
// exempt from the literal type check; the value's ACTUAL type is whatever the
// state_schema declares for the field, which the closed set {string,int,bool,list,map}
// cannot express for a param that writes any of them.
func stateValueParam(desc string) schema.Param {
	return schema.Param{Type: schema.String, Required: true, Description: desc}
}

// modState declares core.state — the keeper-side write point of a service state
// field ([ADR-0084]). The state suffix IS the verb, one address per verb; the verbs
// are ADR-057's, applied by the same engine the retired `state_changes` used.
//
// Every state resolves the field's declared secrets the same way ([ADR-0083] §4 as
// amended by [ADR-0084]): an existing Vault value is KEPT, a missing one is minted
// from a generate_secret() request, and the returned effective value carries each
// secret property as its `vault:` reference. That is what `type: secret` means, not
// what the verb means.
var modState = schema.Module{
	Name: "state",
	States: map[string]schema.State{
		"set": {
			Description: "Keeper-side (on:keeper). Writes the state field named by field, replacing whatever it held. Returns the effective state with each declared secret property replaced by its `vault:` reference ([ADR-0084]).",
			Input: schema.Input{
				"field": paramStateField,
				"value": stateValueParam("The field's whole value. A secret property is written as generate_secret({'length': …, 'charset': …}); omitting it requires the value to already exist."),
			},
		},
		"present": {
			Description: "Keeper-side (on:keeper). Writes the state field named by field ONLY if it has no value yet; an existing value wins, including one written earlier in the same run. Nothing is minted for a write that is discarded.",
			Input: schema.Input{
				"field": paramStateField,
				"value": stateValueParam("The value to write if the field is empty. A secret property is written as generate_secret({'length': …, 'charset': …})."),
			},
		},
		"add": {
			Description: "Keeper-side (on:keeper). Adds ONE element to the collection field named by field, idempotently: an element with the same identity (key or match) is not added twice (ADR-057).",
			Input: schema.Input{
				"field": paramStateField,
				"value": stateValueParam("The element to add. A secret property of the element is written as generate_secret({'length': …, 'charset': …})."),
				"key": {
					Type:        schema.String,
					Description: "Identity of the element in a MAP field: the key it is stored under, an existing entry under the same key being the conflict. Mutually exclusive with match, which is the LIST-field spelling - which of the two the engine reads is decided by the field's kind, so writing the wrong one is an error, not a fallback.",
				},
				"match": {
					Type:        schema.String,
					Description: "Identity of the element in a LIST field: a CEL predicate over `elem` and `value` deciding whether an existing element is the same one, e.g. `elem.name == value.name`. Omitted, identity is deep equality of the whole element. Mutually exclusive with key, which is the MAP-field spelling.",
				},
				"on_conflict": {
					Type: schema.String, Enum: []any{"skip", "replace", "error"},
					Description: "What to do when the element already exists: skip (default, no-op), replace (overwrite it), error (fail the run).",
				},
			},
		},
		"append": {
			Description: "Keeper-side (on:keeper). Appends ONE element to the list field named by field, unconditionally — no identity check. For a sequence where the same element may legitimately occur twice; use add for a set.",
			Input: schema.Input{
				"field": paramStateField,
				"value": stateValueParam("The element to append. A secret property of the element is written as generate_secret({'length': …, 'charset': …})."),
			},
		},
		"modify": {
			Description: "Keeper-side (on:keeper). Patches every element of the collection field named by field that matches match (ADR-057). Writes no value of its own, so it mints no secret.",
			Input: schema.Input{
				"field": paramStateField,
				"match": {
					Type:        schema.String,
					Description: "CEL predicate over `elem` selecting which elements to patch, e.g. `elem.name == 'alice'`. Omitted, it matches NOTHING and the step is a no-op - write `true` to patch every element.",
				},
				"patch": {
					Type: schema.Map, Required: true,
					Description: "Path-inside-the-element -> new value. A value may be a CEL expression over `elem`, so a patch can be computed from what the element already holds.",
				},
				"expect": {
					Type: schema.String, Enum: []any{"any", "one", "at_most_one"},
					Description: "How many elements the match must select: any (default), one, at_most_one. A count outside it fails the run before the commit.",
				},
			},
		},
		"remove": {
			Description: "Keeper-side (on:keeper). Drops every element of the collection field named by field that matches match (ADR-057). Removes elements from inside the field; use unset to drop the field itself.",
			Input: schema.Input{
				"field": paramStateField,
				"match": {
					Type:        schema.String,
					Description: "CEL predicate over `elem` selecting which elements to drop, e.g. `elem.name == 'alice'`. Omitted, it matches NOTHING and the step is a no-op - a forgotten line drops nothing. Write `true` to drop every element, or use core.state.unset to drop the field itself.",
				},
				"expect": {
					Type: schema.String, Enum: []any{"any", "one", "at_most_one"},
					Description: "How many elements the match must select: any (default), one, at_most_one. A count outside it fails the run before the commit.",
				},
			},
		},
		"unset": {
			Description: "Keeper-side (on:keeper). Drops the state field named by field itself, as opposed to remove, which drops elements from inside a collection.",
			Input:       schema.Input{"field": paramStateField},
		},
	},
}
