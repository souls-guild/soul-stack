package harness

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// The dev Vault stores secrets in memory and Postgres stores registries on a volume, so
// a container restart leaves the stand holding databases whose trust anchors are gone.
// Every step of dev/provision.sh is idempotent by its own state, which means an empty
// Vault makes all of them fire: before NIM-365 that quietly minted new anchors under the
// live registries and the stand went silent — tokens answered 401, souls stopped
// establishing mTLS, and nothing said why.
//
// The guard that refuses is shell, and the only property worth pinning is behavioural:
// which combinations of (what Vault has, what the databases remember) refuse and which
// proceed. So these tests lift the real function text out of the script and run it
// against stubbed `vault_cli` / `psql_*`, rather than asserting that some regex appears.
const vaultAnchorGuardFunc = "check_vault_anchors_against_registries"

// The two readers the guard is built from. Extracted alongside it because the guard
// calls them and a stub for them would test the stub.
var vaultAnchorGuardHelpers = []string{"db_row_count", "keeper_databases"}

// registryRow — one table of one database as the stub should answer for it. Absent
// tables are simply not listed: that is how a stand that has never been migrated looks,
// and it is the case a naive `SELECT count(*)` would turn into an error rather than a zero.
type registryRow struct {
	db    string
	table string
	rows  int
}

// degradedCount — the table is there but the count never comes back (dropped connection,
// permission). Distinct from an absent table on purpose: under `set -euo pipefail` an
// unguarded failing pipeline there kills provision with no output at all, and an empty
// result compared against "0" reads as "not zero" and refuses over a registry nobody has
// shown to hold anything. Both are worse than the bug this guard exists to catch.
const degradedCount = -1

// anchorCase — one (what Vault still has, what the databases still remember) combination.
type anchorCase struct {
	name string
	// vaultHas — which anchors survived, as substrings of "jwt pki sigil".
	vaultHas string
	// standDB — the stand being provisioned; empty means the default `keeper`.
	standDB string
	// stackPrefix — the compose project, which `make dev-reset` exports and which names
	// the containers. Empty means the shared `soul-stack`. Deliberately independent of
	// dedicated: stand-env.sh suffixes it with the slug only when DEDICATED_INFRA=1 AND a
	// slug is set, so DEDICATED_INFRA=1 on the shared project is a reachable configuration
	// and not a contradiction.
	stackPrefix string
	rows        []registryRow
	pgUp        bool
	reissue     bool
	// dedicated — DEDICATED_INFRA=1.
	dedicated bool
	// dbListFails — the database list cannot be read at all. Distinct from pgUp: the
	// reachability probe is `pg_isready`, which answers 0 for any server that responds,
	// a rejected connection included.
	dbListFails bool
	wantFail    bool
	wantAny     []string
	wantNever   []string
}

func TestDevProvisionRefusesToMintAnchorsUnderALiveRegistry(t *testing.T) {
	const (
		allAnchors = "jwt pki sigil"
		noAnchors  = ""
	)

	cases := []anchorCase{
		{
			name:      "nothing was lost, so nothing is at stake",
			vaultHas:  allAnchors,
			rows:      []registryRow{{"keeper", "operators", 3}, {"keeper", "soul_seeds", 16}},
			pgUp:      true,
			wantAny:   []string{"trust anchors present"},
			wantNever: []string{"refusing", "destroy"},
		},
		{
			name:      "first-ever stand: vault is empty and so is every registry",
			vaultHas:  noAnchors,
			rows:      nil,
			pgUp:      true,
			wantAny:   []string{"no registry depends on them yet"},
			wantNever: []string{"refusing"},
		},
		{
			name:     "the jwt key is gone and the Archons that its tokens name are not",
			vaultHas: "pki sigil",
			rows:     []registryRow{{"keeper", "operators", 2}},
			pgUp:     true,
			wantFail: true,
			// The refusal has to carry the diagnosis the operator could not get from the
			// symptom: which registry, how many rows, that a 401 is a signature and not a
			// permission, and both ways back.
			wantAny: []string{
				"jwt-signing-key",
				"operators still holds 2",
				"401",
				"make dev-reset",
				"DEV_VAULT_REISSUE_ANCHORS=1",
				"mint-jwt.sh",
				// `keeper` is created once by the postgres container at first init and by
				// nothing afterwards (ensure_stand_db skips it), so dropping it leaves a
				// stand that cannot be provisioned back. The surgical way out genuinely
				// does not exist here, and the refusal has to say why rather than just
				// withhold it.
				"not available on the default stand",
				"cannot be provisioned back",
			},
		},
		{
			name:     "a named stand is told how to drop its own database and nobody else's",
			vaultHas: "pki sigil",
			standDB:  "keeper_nim365",
			rows:     []registryRow{{"keeper_nim365", "operators", 1}},
			pgUp:     true,
			wantFail: true,
			// `make dev-reset` takes the shared Postgres volume down with it — every
			// stand on the host. A stand with its own database has a way out that costs
			// only its own, and that has to be the one offered first.
			wantAny: []string{`DROP DATABASE "keeper_nim365"`, "no other stand touched", "WIPES THE SHARED POSTGRES VOLUME"},
		},
		{
			// The same refusal on a stand that owns its infrastructure. `dev-reset` there
			// is `docker compose down -v` against that stand's own project, so calling it
			// a host-wide wipe would scare an operator off the cheapest correct action and
			// onto the one that keeps the databases and loses the anchors instead.
			name:        "a dedicated stand is told dev-reset costs it nothing but itself",
			vaultHas:    "jwt sigil",
			standDB:     "keeper_nim365",
			stackPrefix: "soul-stack-nim365",
			rows:        []registryRow{{"keeper_nim365", "soul_seeds", 3}},
			pgUp:        true,
			dedicated:   true,
			wantFail:    true,
			wantAny:     []string{"its own docker project", "wipes this stand and nothing else"},
			wantNever:   []string{"WIPES THE SHARED POSTGRES VOLUME"},
		},
		{
			// DEDICATED_INFRA=1 with no DEV_STAND: stand-env.sh only suffixes STACK_PREFIX
			// when there is a slug, so the flag is set while the stand is still the shared
			// `soul-stack` project on the shared `keeper` database. Keying the advice off
			// the flag alone would call `down -v` here "wipes this stand and nothing else"
			// and recommend destroying every neighbour's data as the cheap way out.
			name:        "the dedicated flag without a slug is still the shared stand",
			vaultHas:    "jwt sigil",
			standDB:     "keeper",
			stackPrefix: "soul-stack",
			rows:        []registryRow{{"keeper", "soul_seeds", 4}},
			pgUp:        true,
			dedicated:   true,
			wantFail:    true,
			wantAny:     []string{"WIPES THE SHARED POSTGRES VOLUME", "Check who else is running"},
			wantNever:   []string{"its own docker project", "wipes this stand and nothing else"},
		},
		{
			// The whole PKI half of the guard hangs off one query. A list that failed and a
			// Postgres with nothing on it are the same empty string, so returning it either
			// way lets "psql is broken" read as "no stand depends on the root" — the guard
			// would then wave through exactly the regeneration it exists to stop, on the one
			// anchor that reaches other people's stands.
			name:        "an unreadable database list is UNKNOWN, not empty",
			vaultHas:    "jwt sigil",
			standDB:     "keeper_nim365",
			rows:        []registryRow{{"keeper_nim365", "soul_seeds", 3}},
			pgUp:        true,
			dbListFails: true,
			wantFail:    true,
			wantAny:     []string{"could not be read", "UNKNOWN", "no surgical option here"},
			// Not "nothing depends on it", and no drop-your-own-database advice: with the
			// list unread this stand cannot be shown to be the only one at risk.
			wantNever: []string{"no registry depends on them yet", "DROP DATABASE"},
		},
		{
			name:     "the count never comes back, so provision says so instead of dying or refusing",
			vaultHas: "pki sigil",
			standDB:  "keeper",
			rows:     []registryRow{{"keeper", "operators", degradedCount}},
			pgUp:     true,
			wantFail: false,
			wantAny:  []string{"could not count keeper.operators", "blind to them"},
			// Neither of the two ways this can go wrong: no silent death (the script has
			// to reach its own "nothing depends on them" line), and no refusal built on
			// an empty string that was never a row count.
			wantNever: []string{"[fail]", "Archon(s)"},
		},
		{
			name:      "a neighbour's souls are at stake, so dropping your own database is not offered",
			vaultHas:  "jwt sigil",
			standDB:   "keeper_nim365",
			rows:      []registryRow{{"keeper_nim365", "soul_seeds", 3}, {"keeper_other", "soul_seeds", 16}},
			pgUp:      true,
			wantFail:  true,
			wantAny:   []string{"no surgical option here", "Ask whoever runs it"},
			wantNever: []string{"DROP DATABASE"},
		},
		{
			name:     "the pki root is gone and only ANOTHER stand still has souls",
			vaultHas: "jwt sigil",
			rows:     []registryRow{{"keeper_other", "soul_seeds", 16}},
			pgUp:     true,
			wantFail: true,
			// One Vault serves every stand, so `pki/` is shared: provisioning a brand-new
			// stand would regenerate the root under a neighbour's souls. A guard that only
			// looked at its own PG_DB would wave this through — which is the case that
			// actually costs someone else their fleet.
			wantAny: []string{"keeper_other.soul_seeds", "ANOTHER stand", "re-onboard"},
		},
		{
			name:      "the pki root is gone and no stand has souls yet",
			vaultHas:  "jwt sigil",
			rows:      []registryRow{{"keeper", "operators", 1}},
			pgUp:      true,
			wantAny:   []string{"no registry depends on them yet"},
			wantNever: []string{"refusing"},
		},
		{
			name:     "the sigil key is gone and the grants it signed are not",
			vaultHas: "jwt pki",
			rows:     []registryRow{{"keeper", "plugin_sigils", 4}},
			pgUp:     true,
			wantFail: true,
			wantAny:  []string{"sigil-signing-key", "plugin_sigils still holds 4"},
		},
		{
			name:     "postgres is unreachable, so a fresh stand cannot be told from a wiped one",
			vaultHas: noAnchors,
			rows:     []registryRow{{"keeper", "operators", 2}},
			pgUp:     false,
			// Refusing here would break the first provision of every stand, which is the
			// common case; proceeding silently is what the ticket is about. Say which
			// question went unanswered.
			wantAny:   []string{"postgres is unreachable"},
			wantNever: []string{"refusing"},
		},
		{
			name:     "the loss is accepted on purpose",
			vaultHas: "pki sigil",
			rows:     []registryRow{{"keeper", "operators", 2}},
			pgUp:     true,
			reissue:  true,
			// The escape hatch may not be quieter than the refusal about what it destroys —
			// an operator who set the flag last week still needs to read it today.
			wantAny:   []string{"DEV_VAULT_REISSUE_ANCHORS=1", "operators still holds 2"},
			wantNever: []string{"refusing"},
		},
	}

	guard := provisionShellFunctions(t, append([]string{vaultAnchorGuardFunc}, vaultAnchorGuardHelpers...)...)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := vaultAnchorGuardHarness(guard, tc)
			cmd := exec.Command("bash", "-c", script)
			out, err := cmd.CombinedOutput()
			got := string(out)

			if tc.wantFail && err == nil {
				t.Fatalf("%s let the run continue, so provision would have minted new anchors under a live "+
					"registry — the exact silence NIM-365 is about.\noutput:\n%s", vaultAnchorGuardFunc, got)
			}
			if !tc.wantFail && err != nil {
				t.Fatalf("%s refused a case that costs nothing (%v). A guard that blocks a working stand gets "+
					"disabled, and then it guards nothing.\noutput:\n%s", vaultAnchorGuardFunc, err, got)
			}
			for _, want := range tc.wantAny {
				if !strings.Contains(got, want) {
					t.Errorf("the message never says %q, so whoever hits this still has to guess.\noutput:\n%s", want, got)
				}
			}
			for _, never := range tc.wantNever {
				if strings.Contains(got, never) {
					t.Errorf("the message says %q on a case where nothing is lost.\noutput:\n%s", never, got)
				}
			}
		})
	}
}

// TestDevProvisionChecksAnchorsBeforeItGeneratesThem pins the one ordering the guard
// depends on. It reads registries to decide whether generating is safe, so it is worth
// nothing at all if it runs after the step that generates — the anchors would already be
// new by the time it looked, and it would find them present and skip.
func TestDevProvisionChecksAnchorsBeforeItGeneratesThem(t *testing.T) {
	code := provisionScriptSource(t)

	call := strings.Index(code, "\n"+vaultAnchorGuardFunc+"\n")
	if call < 0 {
		t.Fatalf("%s never calls %s on a line of its own — defining a guard is not running it, and an "+
			"uncalled one leaves provision exactly as silent as before NIM-365",
			devProvisionScript, vaultAnchorGuardFunc)
	}
	generate := strings.Index(code, "generating and writing ${VAULT_KV_PREFIX}/jwt-signing-key")
	if generate < 0 {
		t.Fatalf("%s no longer logs the jwt-signing-key generation this ordering is measured against — "+
			"re-anchor the check on whatever writes the key now", devProvisionScript)
	}
	if call > generate {
		t.Fatalf("%s runs after the jwt-signing-key is generated. By then Vault holds a fresh anchor, the "+
			"guard sees it as present and skips, and the tokens it exists to protect are already dead",
			vaultAnchorGuardFunc)
	}
}

// provisionShellFunctions extracts the named functions from dev/provision.sh in the order
// given, so a test can run the real text instead of a copy of it. A copy would keep
// passing after the script changed, which is the failure mode these guards exist to avoid.
func provisionShellFunctions(t *testing.T, names ...string) string {
	t.Helper()
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, provisionShellFunction(t, name))
	}
	return strings.Join(parts, "\n\n")
}

// vaultAnchorGuardHarness builds a runnable script: the stand variables the guard reads,
// stubs for the two things it asks (Vault, Postgres), then the real guard.
func vaultAnchorGuardHarness(guard string, tc anchorCase) string {
	standDB := tc.standDB
	if standDB == "" {
		standDB = "keeper"
	}
	stackPrefix := tc.stackPrefix
	if stackPrefix == "" {
		stackPrefix = "soul-stack"
	}

	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("VAULT_KV_PREFIX=secret/keeper\n")
	fmt.Fprintf(&b, "PG_DB=%s\n", standDB)
	fmt.Fprintf(&b, "STACK_PREFIX=%s\n", stackPrefix)
	b.WriteString("STAND_DEV_DIR=/tmp/keeper-dev\n")
	fmt.Fprintf(&b, "PG_REACHABLE=%s\n", boolAsFlag(tc.pgUp))
	fmt.Fprintf(&b, "DEV_VAULT_REISSUE_ANCHORS=%s\n", boolAsFlag(tc.reissue))
	fmt.Fprintf(&b, "DEDICATED_INFRA=%s\n", boolAsFlag(tc.dedicated))
	b.WriteString("log() { printf '[provision] %s\\n' \"$*\"; }\n")
	b.WriteString("skip() { printf '[provision] [skip] %s\\n' \"$*\"; }\n")
	b.WriteString("fail() { printf '[provision] [fail] %s\\n' \"$*\" >&2; exit 1; }\n")

	fmt.Fprintf(&b, "VAULT_HAS=%q\n", tc.vaultHas)
	b.WriteString(`vault_cli() {
    case "$*" in
        *jwt-signing-key*)   case " $VAULT_HAS " in *" jwt "*)   return 0 ;; esac ;;
        *sigil-signing-key*) case " $VAULT_HAS " in *" sigil "*) return 0 ;; esac ;;
        *pki/cert/ca*)       case " $VAULT_HAS " in *" pki "*)   return 0 ;; esac ;;
    esac
    return 1
}
`)

	// row_count answers "how many rows", ABSENT for "no such table" - which is what
	// db_row_count has to tell apart with two queries - or DEGRADED for a table that
	// is there while the count itself does not come back.
	b.WriteString("row_count() {\n    case \"$1.$2\" in\n")
	for _, r := range tc.rows {
		if r.rows == degradedCount {
			fmt.Fprintf(&b, "        %s.%s) printf 'DEGRADED' ;;\n", r.db, r.table)
			continue
		}
		fmt.Fprintf(&b, "        %s.%s) printf '%d' ;;\n", r.db, r.table, r.rows)
	}
	b.WriteString("        *) printf 'ABSENT' ;;\n    esac\n}\n")

	b.WriteString(`psql_db() {
    local db="$1"; shift
    local q="$*" table="" n
    case "$q" in
        *operators*)     table=operators ;;
        *soul_seeds*)    table=soul_seeds ;;
        *plugin_sigils*) table=plugin_sigils ;;
    esac
    n="$(row_count "$db" "$table")"
    case "$q" in
        *to_regclass*) if [ "$n" = ABSENT ]; then printf 'f\n'; else printf 't\n'; fi ;;
        *count*)       if [ "$n" = DEGRADED ]; then return 1; fi; printf '%s\n' "$n" ;;
    esac
}
`)
	// psql_admin is the guard's one window onto which stands exist. Failing means failing
	// the way psql does — a status and nothing on stdout — because the two are the same
	// empty string to the caller and only the status tells them apart.
	if tc.dbListFails {
		b.WriteString("psql_admin() { return 2; }\n")
	} else {
		b.WriteString("psql_admin() { printf '%s\\n' keeper keeper_other keeper_nim365; }\n")
	}

	b.WriteString("\n")
	b.WriteString(guard)
	b.WriteString("\n\n")
	b.WriteString(vaultAnchorGuardFunc + "\n")
	return b.String()
}

func boolAsFlag(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
