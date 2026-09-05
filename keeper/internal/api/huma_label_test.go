package api

// Wire contract of the shared label body, [ADR-0085] / NIM-728. One field, and
// its two properties are BOTH deliberate and neither is huma's default:
//
//   - OPTIONAL — `{}` is a valid body and clears the caption. huma marks a field
//     required unless told otherwise, and the default would have made an empty
//     body a 422 on REST while the MCP twin went on accepting an omitted `label`
//     — the two primary operator surfaces (ADR-004) disagreeing about what
//     clearing a caption looks like.
//   - NULLABLE — `{"label": null}` is the explicit spelling of "clear it", and
//     the one the MCP schema accepts. Reaching optionality through
//     `json:",omitempty"` instead of `required:"false"` would have dropped the
//     `"null"` arm of the generated type and made this body a schema violation.
//
// Both spellings are exercised here rather than asserted in the spec alone,
// because the spec is derived: a wrong tag produces a spec that agrees with
// itself and refuses a caller.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// TestLabelSetRequest_ClearingSpellings drives the real route with each way of
// saying "no caption" and pins that both are accepted.
func TestLabelSetRequest_ClearingSpellings(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"explicit null", `{"label":null}`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := &hHeraldPool{heraldUpdateRows: 1}
			r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s\n"+
					"ADR-0085: `%s` must clear the caption. A 422 here means the generated schema "+
					"disagrees with the documented contract and with the MCP twin.",
					rec.Code, rec.Body.String(), tc.body)
			}
			// What the 200 above pins is the CONTRACT: both spellings pass schema
			// validation and reach the handler. That the write then stores NULL is
			// the domain's business and is pinned there (registrylabel.Normalize and
			// each registry's UpdateLabel) — the fixture pool replays one fixed row,
			// so the reply's caption says nothing about what was written.
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal reply: %v", err)
			}
			if _, present := got["label"]; present {
				t.Errorf("the reply carried `label` = %v for a row whose caption is NULL; "+
					"an absent caption must be an absent key, or a consumer cannot tell "+
					"\"no caption\" from an empty one", got["label"])
			}
			if got["id"] != "ops-webhook" {
				t.Errorf("reply `id` = %v, want the identifier — a label-set must not touch it", got["id"])
			}
		})
	}
}

// TestLabelSetRequest_SetsAFreeTextCaption pins the other direction: capitals,
// spaces and punctuation go through untouched, because the narrow grammar
// belongs to the identifier and not to the caption.
func TestLabelSetRequest_SetsAFreeTextCaption(t *testing.T) {
	// Its own literal, not createCaption: this test is ABOUT the characters, so
	// narrowing the shared constant later must not quietly narrow what it proves.
	const caption = "Ops — Billing Webhook (prod)" //nolint:goconst // see above

	pool := &hHeraldPool{heraldUpdateRows: 1}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"label": caption})
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The fixture pool replays a fixed row, so the reply's caption is whatever
	// heraldScanRow carries rather than what was sent; what this pins is that a
	// caption with capitals, spaces and an em-dash is ACCEPTED — no pattern, no
	// length bound, nothing to fail.
	if strings.Contains(rec.Body.String(), "validation") {
		t.Errorf("a free-text caption was rejected: %s", rec.Body.String())
	}
}

// createCaption — the caption the create tests send. Capitals, spaces, an em-dash
// and parentheses: nothing a registry identifier could be, so a value bound to the
// wrong column would be refused by the domain rather than pass unnoticed.
const createCaption = "Ops — Billing Webhook (prod)"

// writeProbe records the statement a fixture pool was asked to run, and its bind
// arguments.
//
// Recorded because a REPLY cannot answer these questions on its own. Every one of
// these registries builds its create body from the in-memory entity rather than
// re-reading the row, so a caption that reached the reply may still have stopped
// short of the write; and on the label-set route the reply is re-read through the
// fixture's own hardcoded row, so it echoes the fixture rather than the write and
// is worth nothing as evidence about what the UPDATE did.
//
// The statement is kept beside the arguments because half the mutations worth
// catching live in one and half in the other: dropping `label` from an INSERT's
// column list while leaving the argument in place, or adding `id` to a label-set's
// SET clause while the binds stay as they were.
//
// Embedded into each domain's pool fake. Each test builds its own pool, so a pool
// with two recording arms (heralds and tidings) never has to keep them apart.
type writeProbe struct {
	insertSQL  string
	insertArgs []any
	updateSQL  string
	updateArgs []any
	// execSQL — every statement the pool ran through Exec, in order. A route is
	// judged by what it wrote in TOTAL, not only by the one statement a test
	// thought to look at: pinning the shape of the caption UPDATE says nothing
	// about a SECOND write issued beside it.
	execSQL []string
}

func (p *writeProbe) recordInsert(sql string, args []any) {
	p.insertSQL, p.insertArgs = sql, args
}

func (p *writeProbe) recordUpdate(sql string, args []any) {
	p.updateSQL, p.updateArgs = sql, args
}

func (p *writeProbe) recordExec(sql string) { p.execSQL = append(p.execSQL, sql) }

// createCaptionCase — one create route driven end to end for [NIM-817].
type createCaptionCase struct {
	// route and body as a client would send them, caption included.
	path string
	body map[string]any
	// router builds the production route over a fixture whose probe is returned
	// beside it, and wantStatus is what that route answers on success.
	router     func(t *testing.T) (http.Handler, *writeProbe)
	wantStatus int
	// table is the one the INSERT must have addressed. Checked, not decoration:
	// two of these fixtures record from more than one INSERT arm, so without it a
	// route that wrote its sibling table would satisfy the assertion.
	table string
	// replyCarriesRow is false where the route answers with an acknowledgement
	// rather than the row — incarnation create is asynchronous (202 + apply_id),
	// so its caption is read back by a later GET and only the write is checked here.
	replyCarriesRow bool
}

// TestLabelOnCreate_ReachesTheWriteAndTheReply is the behavioural half of
// [NIM-817]: the caption sent in a CREATE body reaches the write, on EVERY one of
// the eight registries.
//
// Every route, not a sample, and over HTTP rather than through the projection
// function — because those are two different claims. The projection guard
// (huma_wire_projection_guard_test.go) proves each function carries the field; only
// driving the route proves the route CALLS that function. A registration closure
// that quietly stopped using its projection would leave the other guard green and
// ship exactly the defect this ticket is about.
//
// Both ends are asserted per route. The reply is projected from the in-memory
// entity, so a reply carrying the caption proves only that the wire mapping kept
// it — a write that dropped it on the way to SQL would answer 201 with the caption
// in the body and store NULL. The INSERT proves the other end. Together they are
// "the client's value reached the row".
//
// HOW TO BREAK IT ON PURPOSE (the mutations this test exists to catch): delete the
// `Label:` line from any of the eight functions in huma_create_input.go, or make
// any of the eight registration closures in huma_<domain>.go stop calling its
// projection. That route's subtest goes red — which is EXACTLY the shipped
// behaviour this ticket found: a 2xx that told the caller nothing was wrong.
func TestLabelOnCreate_ReachesTheWriteAndTheReply(t *testing.T) {
	cases := map[string]createCaptionCase{
		"service": {
			path: "/v1/services",
			body: map[string]any{"id": "web", "git": "https://git/web.git", "ref": "v1.0.0"},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hSvcPool{}
				return humaServiceRouter(t, strictAllowAll{}, nil, p, nil, nil, nil, nil), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "service_registry", replyCarriesRow: true,
		},
		"herald": {
			path: "/v1/heralds",
			body: map[string]any{"id": "ops-webhook", "type": "webhook",
				"config": map[string]any{"url": "https://hook.test/notify"}},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hHeraldPool{}
				return humaHeraldRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "heralds", replyCarriesRow: true,
		},
		"tiding": {
			path: "/v1/tidings",
			body: map[string]any{"id": "on-fail", "herald": "ops-webhook",
				"event_types": []string{"scenario_run.*"}},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hHeraldPool{}
				return humaHeraldRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "tidings", replyCarriesRow: true,
		},
		"vigil": {
			path: "/v1/vigils",
			body: map[string]any{"id": "web-conf", "subject": map[string]any{"coven": []string{"web"}},
				"interval": "30s", "check": "core.beacon.file_changed"},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hOraclePool{}
				return humaOracleRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "vigils", replyCarriesRow: true,
		},
		"decree": {
			path: "/v1/decrees",
			body: map[string]any{"id": "on-conf", "on_beacon": "web-conf",
				"subject":          map[string]any{"coven": []string{"web"}},
				"incarnation_name": "web", "action_scenario": "reload"},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hOraclePool{}
				return humaOracleRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "decrees", replyCarriesRow: true,
		},
		"omen": {
			path: "/v1/augur/omens",
			body: map[string]any{"id": "vault-prod", "source_type": "vault",
				"endpoint": "https://vault:8200", "auth_ref": "vault:secret/keeper/ar"},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := &hAugurPool{}
				return humaAugurRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "omens", replyCarriesRow: true,
		},
		"push-provider": {
			path: "/v1/push-providers",
			body: map[string]any{"id": "vault-bastion",
				"params": map[string]any{"vault_addr": "https://vault.example.com"}},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				p := newHPushProviderPool()
				return humaPushProviderRouter(t, strictAllowAll{}, nil, p), &p.writeProbe
			},
			wantStatus: http.StatusCreated, table: "push_providers", replyCarriesRow: true,
		},
		"incarnation": {
			path: "/v1/incarnations",
			body: map[string]any{"id": "web-01", "service": "web"},
			router: func(t *testing.T) (http.Handler, *writeProbe) {
				// runner=nil → stub mode: the create lands the row and returns 202
				// without resolving a service or starting a bootstrap run, which is
				// all this subtest needs to see the INSERT.
				db := &incTestDB{}
				incH := handlers.NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil, nil)
				return humaIncarnationRouter(t, incEnforcer{allow: true}, nil, incH), &db.writeProbe
			},
			wantStatus: http.StatusAccepted, table: "incarnation", replyCarriesRow: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, probe := tc.router(t)
			body := map[string]any{"label": createCaption}
			for k, v := range tc.body {
				body[k] = v
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(raw)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.replyCarriesRow {
				assertCaptionEchoed(t, rec.Body.Bytes())
			}
			assertCaptionWritten(t, probe, tc.table)
		})
	}
}

// assertCaptionEchoed pins that the create reply carries back the caption it was
// given. A caller has no other way to tell a stored caption from a discarded one
// without a second request.
func assertCaptionEchoed(t *testing.T, reply []byte) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(reply, &got); err != nil {
		t.Fatalf("reply is not a JSON object: %v; body=%s", err, reply)
	}
	switch v, present := got["label"]; {
	case !present:
		t.Errorf("the reply carries NO `label` for a create that sent one ([NIM-817]): %s\n"+
			"The schema declares the field and the body validated, so the caller has a 2xx and "+
			"believes the caption is saved.", reply)
	case v != createCaption:
		t.Errorf("reply `label` = %q, want %q", v, createCaption)
	}
}

// assertCaptionWritten pins the other end: the INSERT named the caption's column
// AND was handed the caption.
//
// Both, because either alone is passable by a broken write. Binding the value to a
// statement that no longer lists `label` is a placeholder-count error real Postgres
// would refuse but a fixture will not notice; naming the column and binding
// something else is the drop this ticket is about.
//
// Membership rather than a bind POSITION — column order belongs to the repository
// and is pinned by its own tests, while what matters here is that the value crossed
// every layer between the wire and the SQL. What membership does NOT establish is
// WHICH column received it: nothing here would notice a caption bound to some other
// text column. On seven of the eight routes the reply check closes that (a reply
// carrying the caption came from the entity's own Label field); on `incarnation`,
// whose 202 carries no row, it stays open.
func assertCaptionWritten(t *testing.T, probe *writeProbe, table string) {
	t.Helper()
	if probe.insertArgs == nil {
		t.Fatalf("no INSERT INTO %s was executed — the fixture recorded no statement, "+
			"so this subtest would pass without the route ever writing", table)
	}
	// `INTO <table> (` and not `INTO <table>`: four real tables have `incarnation`
	// as a prefix, so the looser form would accept a write to incarnation_membership
	// as a write to incarnation.
	if !strings.Contains(probe.insertSQL, "INTO "+table+" (") {
		t.Fatalf("the recorded statement is not an INSERT INTO %s — the route wrote "+
			"somewhere else, and checking its binds would prove nothing:\n%s", table, probe.insertSQL)
	}
	if !strings.Contains(probe.insertSQL, "label") {
		t.Errorf("INSERT INTO %s does not name the `label` column:\n%s", table, probe.insertSQL)
	}
	// Either spelling of a nullable text bind: the repositories differ on whether
	// they hand pgx the string or a *string, and both mean the same thing to the
	// column.
	for _, a := range probe.insertArgs {
		switch v := a.(type) {
		case string:
			if v == createCaption {
				return
			}
		case *string:
			if v != nil && *v == createCaption {
				return
			}
		}
	}
	t.Errorf("the caption was NOT bound to INSERT INTO %s; args = %#v\n"+
		"The route answered 2xx, so the caller was told the caption was saved. "+
		"It got as far as the reply and stopped there ([NIM-817]).", table, probe.insertArgs)
}

// TestLabelSet_DoesNotTouchTheIdentifier pins the half of [ADR-0085] that makes the
// two fields safe to have at once: changing the caption addresses the row by its
// IDENTIFIER and rewrites nothing else.
//
// The identifier is segment 2 of every derived secret path and the value of an
// `incarnation=` RBAC scope, so a label-set that touched it would silently
// re-address a channel's secrets and a role's grant. There is no rename operation
// anywhere in the registries, and a label-set — one of the few places where a
// caption and an identifier are both in hand — must not become one by accident.
//
// Asserted on the STATEMENT, not on the reply. The reply is re-read through the
// fixture's own hardcoded row, so its `id` is the constant the fixture carries
// whatever the UPDATE did — a check there is green by construction and says
// nothing. What the route actually did is in the SET clause and the binds.
//
// HOW TO BREAK IT ON PURPOSE, and all four are traced red: add `, id = $2` to the
// SET clause of heraldUpdateLabelSQL (herald/crud.go); point it at `UPDATE tidings`
// instead (the two constants are adjacent and identical, so this is the plausible
// copy-paste); change `WHERE x.id = $1` to `WHERE x.label = $1`; or add a second
// `db.Exec(... "UPDATE heralds SET id = ...")` beside the caption write.
//
// SCOPE, so a green result is not read for more than it says: this drives HERALD.
// The other seven label-set statements (serviceregistry/repository.go,
// oracle/crud.go x2, augur/crud.go, pushprovider/crud.go, incarnation/crud.go,
// herald/crud.go's tiding twin) are structurally identical — `UPDATE <t> AS x SET
// label = $2 FROM <t> AS old WHERE x.id = $1 RETURNING old.label` — but each is
// its own constant, so the same mutation applied to one of THEM is not caught
// here. Widening this means recording the caption UPDATE in five more fixture
// pools; it was judged out of scope for a ticket about the create path.
func TestLabelSet_DoesNotTouchTheIdentifier(t *testing.T) {
	const id = "ops-webhook"

	pool := &hHeraldPool{heraldUpdateRows: 1}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"label": createCaption})
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/"+id+"/label", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if pool.updateSQL == "" {
		t.Fatal("no caption UPDATE was recorded — the route answered 200 without writing, " +
			"and every assertion below would pass vacuously")
	}

	// The statement addresses the caller's OWN registry. The herald and tiding
	// caption statements are adjacent and identical but for the table name.
	if !strings.Contains(pool.updateSQL, "UPDATE heralds ") {
		t.Errorf("the caption UPDATE addresses the wrong table — a herald label-set "+
			"rewrote something else:\n%s", pool.updateSQL)
	}

	assigned := assignedColumns(t, pool.updateSQL)
	if len(assigned) != 1 || assigned[0] != "label" {
		t.Errorf("the caption UPDATE assigns %v, want exactly [label] — a caption edit must "+
			"rewrite the caption and nothing else ([ADR-0085]). The identifier derives Vault "+
			"paths and RBAC scope values and has no rename operation.\n%s", assigned, pool.updateSQL)
	}

	// The row is addressed by the identifier from the PATH, and by the IDENTIFIER
	// COLUMN. Both halves are needed: binding the right value against `WHERE
	// x.label = $1` would key the endpoint on a mutable, non-unique column and
	// match zero-or-many rows, with the binds looking untouched.
	if len(pool.updateArgs) == 0 || pool.updateArgs[0] != id {
		t.Errorf("the caption UPDATE was keyed on %#v, want the path identifier %q",
			pool.updateArgs, id)
	}
	if !strings.Contains(pool.updateSQL, "x.id = $1") {
		t.Errorf("the caption UPDATE does not key on the identifier column:\n%s", pool.updateSQL)
	}

	// "rewrites nothing else" is about the ROUTE, not about one statement: a second
	// write issued beside the caption UPDATE would break the invariant while every
	// assertion above stayed green.
	if len(pool.execSQL) != 0 {
		t.Errorf("the caption route issued %d further statement(s) besides the caption "+
			"UPDATE: %q\nA label-set writes once ([ADR-0085]).", len(pool.execSQL), pool.execSQL)
	}
}

// assignedColumns returns the column names an UPDATE's SET clause writes to.
//
// Deliberately shallow: these statements are constants in the repositories, one
// assignment per column, and a parser that understood more would be a second
// implementation to keep correct. It reads up to the first clause keyword after
// SET, so the `FROM … WHERE` self-join these label statements use for RETURNING
// the previous value is not mistaken for an assignment.
func assignedColumns(t *testing.T, sql string) []string {
	t.Helper()
	// ASCII-only upper, byte for byte: the offsets found here index into `sql`, and
	// unicode.ToUpper does not preserve byte length. SQL keywords are ASCII; a
	// non-ASCII comment inside a future constant would otherwise shift the slice.
	upper := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}, sql)
	start := strings.Index(upper, "SET ")
	if start < 0 {
		t.Fatalf("no SET clause in the recorded statement:\n%s", sql)
	}
	rest := sql[start+len("SET "):]
	restUpper := upper[start+len("SET "):]
	end := len(rest)
	for _, kw := range []string{"\nFROM ", "\nWHERE ", "\nRETURNING ", " FROM ", " WHERE ", " RETURNING "} {
		if i := strings.Index(restUpper, kw); i >= 0 && i < end {
			end = i
		}
	}
	var cols []string
	for _, assignment := range strings.Split(rest[:end], ",") {
		name, _, ok := strings.Cut(assignment, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		// `x.label` and `label` are the same column; the alias is noise here.
		if _, after, qualified := strings.Cut(name, "."); qualified {
			name = after
		}
		cols = append(cols, name)
	}
	return cols
}

// TestLabelSetRequest_UnknownFieldRejected keeps the body closed: the endpoint
// takes one field, and a caller who misspells it must hear so rather than have
// the write silently succeed as a clear.
func TestLabelSetRequest_UnknownFieldRejected(t *testing.T) {
	pool := &hHeraldPool{heraldUpdateRows: 1}
	r := humaHeraldRouter(t, strictAllowAll{}, nil, pool)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/heralds/ops-webhook/label",
		strings.NewReader(`{"labell":"typo"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Errorf("a misspelled field was accepted (status 200) and would have silently CLEARED "+
			"the caption; body=%s", rec.Body.String())
	}
}
