package api

// The wire→native projection of every create body, in one place ([NIM-817]).
//
// Each create route binds a request struct declared for huma (the schema and its
// validation) and calls a handler with a DIFFERENT struct — the handler-native
// input. Between the two sits a hand-written field list, and a hand-written field
// list is a place a field can be silently omitted: the body validates, the route
// answers 201, and the value never reaches the write. That is exactly how `label`
// was lost on all eight registries at once — the schema declared it, the domain
// stored it, the handler accepted it, and these eight literals did not mention it.
//
// The projections live here rather than inline in the huma.Register closures so
// that a test can enumerate them. The omission was never UNDETECTABLE — the
// closures are reachable over HTTP through each domain's router harness, and
// huma_label_test.go now drives all eight that way. What an inline literal cannot
// do is be reflected over: there was nothing to point a guard at that would fail
// for a field nobody thought to send. As named functions they are driven by
// huma_wire_projection_guard_test.go, which fills the request twice — every field
// at once with a value unique to it, then one field at a time — and fails on any
// native field that does not arrive holding it. So the NEXT field added to a create
// body cannot be dropped, or sourced from the wrong one, without a red test.
//
// That guard is no longer about these eight: [NIM-824] extended it to every
// hand-written wire→native projection in the package, and it derives the list from
// the package's own source, so a projection written anywhere here is covered
// whether or not it lives in this file. [NIM-831] then extracted the thirty
// mappings still written inline into huma_request_input.go, the sibling of this
// file — so there is no longer any form of wire→native mapping here that the guard
// only counts.
//
// Both guards are needed and neither subsumes the other: this file's functions can
// be correct while a route stops calling one, which is why huma_label_test.go
// drives the routes and not these.
//
// A field a projection deliberately does not carry is therefore a decision to
// state, not an omission to make quietly: name it in the guard's `unmapped` set
// with the reason.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// toServiceRegisterInput projects the POST /v1/services body onto the handler input.
func toServiceRegisterInput(b ServiceRegisterRequest) handlers.ServiceRegisterInput {
	return handlers.ServiceRegisterInput{
		ID:      b.ID,
		Label:   b.Label,
		Git:     b.Git,
		Ref:     b.Ref,
		Refresh: b.Refresh,
	}
}

// toIncarnationCreateInput projects the POST /v1/incarnations body onto the handler input.
func toIncarnationCreateInput(b IncarnationCreateRequest) handlers.IncarnationCreateRequestInput {
	return handlers.IncarnationCreateRequestInput{
		ID:             b.ID,
		Label:          b.Label,
		Service:        b.Service,
		Covens:         b.Covens,
		Input:          b.Input,
		Traits:         b.Traits,
		CreateScenario: b.CreateScenario,
	}
}

// toHeraldCreateInput projects the POST /v1/heralds body onto the handler input.
func toHeraldCreateInput(b HeraldCreateRequest) handlers.HeraldCreateInput {
	return handlers.HeraldCreateInput{
		ID:        b.ID,
		Label:     b.Label,
		Type:      b.Type,
		Config:    b.Config,
		SecretRef: b.SecretRef,
		Secret:    b.Secret,
		Enabled:   b.Enabled,
	}
}

// toTidingCreateInput projects the POST /v1/tidings body onto the handler input.
func toTidingCreateInput(b TidingCreateRequest) handlers.TidingCreateInput {
	return handlers.TidingCreateInput{
		ID:           b.ID,
		Label:        b.Label,
		Herald:       b.Herald,
		EventTypes:   b.EventTypes,
		OnlyFailures: b.OnlyFailures,
		OnlyChanges:  b.OnlyChanges,
		Incarnation:  b.Incarnation,
		Cadence:      b.Cadence,
		Task:         b.Task,
		Annotations:  b.Annotations,
		Projection:   b.Projection,
		Enabled:      b.Enabled,
	}
}

// toVigilCreateInput projects the POST /v1/vigils body onto the handler input.
func toVigilCreateInput(b VigilCreateRequest) handlers.VigilCreateInput {
	return handlers.VigilCreateInput{
		ID:       b.ID,
		Label:    b.Label,
		Subject:  b.Subject.selector(),
		Interval: b.Interval,
		Check:    b.Check,
		Params:   b.Params,
		Enabled:  b.Enabled,
	}
}

// toDecreeCreateInput projects the POST /v1/decrees body onto the handler input.
func toDecreeCreateInput(b DecreeCreateRequest) handlers.DecreeCreateInput {
	return handlers.DecreeCreateInput{
		ID:              b.ID,
		Label:           b.Label,
		OnBeacon:        b.OnBeacon,
		Subject:         b.Subject.selector(),
		IncarnationName: b.IncarnationName,
		ActionScenario:  b.ActionScenario,
		ActionInput:     b.ActionInput,
		Where:           b.Where,
		Cooldown:        b.Cooldown,
		Enabled:         b.Enabled,
	}
}

// toOmenCreateInput projects the POST /v1/augur/omens body onto the handler input.
func toOmenCreateInput(b OmenCreateRequest) handlers.OmenCreateInput {
	return handlers.OmenCreateInput{
		ID:         b.ID,
		Label:      b.Label,
		SourceType: b.SourceType,
		Endpoint:   b.Endpoint,
		AuthRef:    b.AuthRef,
	}
}

// toPushProviderCreateInput projects the POST /v1/push-providers body onto the
// handler input.
//
// Params changes shape across the boundary: the body carries a plain map, the
// handler a *map, because there "omitted" and "an empty set" are different
// instructions to the write. An absent map stays nil rather than becoming a pointer
// to an empty one. (It is not the only such field on these eight — `Subject`
// becomes a subject.Selector in the vigil and decree projections.)
func toPushProviderCreateInput(b PushProviderCreateRequest) handlers.PushProviderCreateInput {
	in := handlers.PushProviderCreateInput{
		ID:    b.ID,
		Label: b.Label,
	}
	if b.Params != nil {
		p := b.Params
		in.Params = &p
	}
	return in
}
