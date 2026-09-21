package cel

import "testing"

// ★ NIM-826 — the addressing half. `incarnation.state.<field>` arrives at
// [selectBaseField] as the pair ("incarnation", "state"), one level shallower
// than the name the author is reading, and the taint has to be decided on the
// FIELD: sealing the pair would seal every cell reading any state field there is.
//
// The producer half (which fields the manifest declares secret) is render's, and
// is tested through the real pipeline in
// keeper/internal/render/seal_hop_test.go — a detector test cannot see a
// pipeline that hands it the wrong set, which is what every hop in this family
// turned out to be.
func TestDetectSealed_IncarnationStateField(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "db_password"): true,
	}}

	for _, expr := range []string{
		"${ incarnation.state.db_password }",
		"redis-cli -a ${ incarnation.state.db_password }", // the ticket's own cell
		`${ incarnation["state"].db_password }`,           // the index spelling of the same map
		"${ has(incarnation.state.db_password) ? incarnation.state.db_password : '' }",
	} {
		if !e.DetectSealed(expr, src) {
			t.Errorf("%q is NOT sealed -- a `secret: true` state field renders plaintext into apply_run_plan", expr)
		}
	}
}

// ★ The other half of the fix, and the one a total seal would pass: a state
// field that is not declared secret must NOT be sealed. Almost every cell in a
// real scenario reads some state field, so sealing the `incarnation.state`
// binding whole would mask the run's diagnostics wholesale and no test asserting
// only the positive case would notice.
func TestDetectSealed_NonSecretStateFieldIsNotSealed(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "db_password"): true,
	}}

	for _, expr := range []string{
		"${ incarnation.state.port }",
		"${ incarnation.state.cluster_size }",
		"port ${ incarnation.state.port } of ${ incarnation.state.cluster_size }",
	} {
		if e.DetectSealed(expr, src) {
			t.Errorf("%q is sealed -- the taint is per state FIELD, and a mask covering every state read says nothing", expr)
		}
	}
}

// The two levels are separate namespaces, and the address spelling is what keeps
// them apart: a state field named `id` is `incarnation.state.id`, the registry
// metadata is `incarnation.id`. Collapsing the flattened read onto the
// `incarnation` root would seal every `${ incarnation.id }` in the run for a
// service that happens to keep an `id` in its state.
func TestDetectSealed_StateFieldDoesNotSealTheMetadataOfTheSameName(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "id"):      true,
		FieldAddr(IncarnationStateAddrRoot, "service"): true,
	}}

	if !e.DetectSealed("${ incarnation.state.id }", src) {
		t.Error("the state field is not sealed")
	}
	for _, expr := range []string{"${ incarnation.id }", "${ incarnation.service }", "${ incarnation.host_count }"} {
		if e.DetectSealed(expr, src) {
			t.Errorf("%q is sealed -- registry metadata was masked because a state field shares its name", expr)
		}
	}
}

// A secret nested below a state field (`tls.key`) is addressable only at its top
// segment, so the seal covers the whole `tls` subtree. Deliberate and stated:
// CEL never names the leaf on its own, and the subtree rule is what the rest of
// the seal already does with a resolved `vault:` map.
func TestDetectSealed_IncarnationStateSubtree(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "tls"): true,
	}}

	for _, expr := range []string{
		"${ incarnation.state.tls }",
		"${ incarnation.state.tls.key }",
		"${ incarnation.state.tls.chain.intermediate }",
	} {
		if !e.DetectSealed(expr, src) {
			t.Errorf("%q is NOT sealed -- a sealed subtree has no unsealed interior", expr)
		}
	}
	if e.DetectSealed("${ incarnation.state.tls_enabled }", src) {
		t.Error("a sibling whose name merely starts with the sealed one was sealed -- the address is a field, not a prefix")
	}
}

// The whole-map read is NOT closed here, exactly as `${ input }` is not: it is
// the same property of every per-field address base and is NIM-827. Pinned so
// the bound is visible rather than assumed, and so closing NIM-827 has to come
// through this line rather than past it.
//
// `${ incarnation }` is pinned beside `${ incarnation.state }` because it is the
// WIDER of the two and the easier one to forget: the activation root carries the
// state map whole ([Vars.incarnationRoot]), so a bare read of it takes home every
// state secret one level above the read the narrow case names.
func TestDetectSealed_IncarnationStateWholeMapIsItsOwnQuestion(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "db_password"): true,
	}}

	for _, expr := range []string{"${ incarnation.state }", "${ incarnation }"} {
		if e.DetectSealed(expr, src) {
			t.Errorf("%q seals now. That is NIM-827 and a WIDENING, not a bug fix to leave unremarked:\n"+
				"it seals every cell reading the whole map for any service declaring one state secret.\n"+
				"Close NIM-827 and update this line and TestDetectSealed_BareFixedRootIsNotWholeTainted\n"+
				"together -- the two are one question.", expr)
		}
	}
}

// The INDEXED spelling of a field read is not sealed either, for any root — CEL
// treats `x.y` and `x["y"]` as the same read, but only the first is a Select node
// and the detector reaches no other kind. Open as NIM-830.
//
// Pinned here rather than left to the ticket text because this file is where the
// index form is reasoned about at all: [isIncarnationState] accepts it in the
// OPERAND position (`incarnation["state"].pw`), and a reader who sees that arm has
// every reason to assume the field position is covered too. It is not.
func TestDetectSealed_IndexedFieldReadIsItsOwnQuestion(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr(IncarnationStateAddrRoot, "db_password"): true,
		FieldAddr("input", "password"):                     true,
	}}

	// The operand position IS closed, and the two assertions belong together: this
	// one is what makes the next one surprising.
	if !e.DetectSealed(`${ incarnation["state"].db_password }`, src) {
		t.Error("the index spelling of the STATE binding is not sealed -- that arm is this ticket's, not NIM-830's")
	}

	for _, expr := range []string{
		`${ incarnation.state["db_password"] }`,
		`${ incarnation["state"]["db_password"] }`,
		`${ input["password"] }`,
	} {
		if e.DetectSealed(expr, src) {
			t.Errorf("%q seals now -- NIM-830 is closed. Good, but it is a widening for every root at once:\n"+
				"update this line and the bound stated in docs/templating.md rather than deleting it.", expr)
		}
	}
}
