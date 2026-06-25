// GOLDEN byte-exact wire-guard для NATIVE wire-DTO SOUL-домена (handler-native T5d). soul
// больше НЕ зависит от legacy-генерата — golden сверяет json native-значения с ЗАФИКСИРОВАННОЙ
// строкой-эталоном (pinned). Гарантирует wire-форму:
//
//   - категория A (date-time): registered_at/expires_at/started_at/last_seen_at → RFC3339Nano;
//   - категория B ([]-vs-null): covens (SoulListEntry) — БЕЗ omitempty → `[]`/значение;
//     traits (SoulListEntry) — map БЕЗ omitempty → `{}`/object (handler coalesce → `{}`);
//   - категория C (omitempty): bootstrap_token/covens(create)/expires_at; history-поля
//     finished_at/incarnation/module/scenario/voyage_id — ключ опущен при nil;
//   - категория D (nullable): SoulListEntry.created_by_aid/last_seen_at/last_seen_by_kid/
//     requested_at — БЕЗ omitempty → `null` при nil;
//   - enum status/transport/type: тот же string-байт.
//
// Покрыты обе указательных ветки (full / nil-optionals). Мутация формы native-struct краснит case.
package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

func goldenSoulWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_SoulReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	tok := "BOOTSTRAP.TOKEN.PLAIN"
	covens := []string{"prod", "eu"}

	// --- SoulCreateReply: transport=agent (token+expires+covens заданы) ---
	goldenSoulWire(t, "CreateReply/agent_full",
		SoulCreateReply{BootstrapToken: &tok, Covens: &covens, CreatedByAID: "archon-alice", ExpiresAt: &ts2, RegisteredAt: ts, SID: "web1.example.com", Status: SoulStatus("pending"), Transport: SoulTransport("agent")},
		`{"bootstrap_token":"BOOTSTRAP.TOKEN.PLAIN","covens":["prod","eu"],"created_by_aid":"archon-alice","expires_at":"2026-06-13T01:02:03.456789012Z","registered_at":"2026-06-14T12:34:56.789012345Z","sid":"web1.example.com","status":"pending","transport":"agent"}`)
	// --- SoulCreateReply: transport=ssh (token/expires/covens опущены — omitempty) ---
	goldenSoulWire(t, "CreateReply/ssh_nil_optionals",
		SoulCreateReply{BootstrapToken: nil, Covens: nil, CreatedByAID: "archon-bob", ExpiresAt: nil, RegisteredAt: ts, SID: "db1.example.com", Status: SoulStatus("pending"), Transport: SoulTransport("ssh")},
		`{"created_by_aid":"archon-bob","registered_at":"2026-06-14T12:34:56.789012345Z","sid":"db1.example.com","status":"pending","transport":"ssh"}`)

	// --- SoulIssueTokenReply (все required) ---
	goldenSoulWire(t, "IssueTokenReply",
		SoulIssueTokenReply{BootstrapToken: tok, ExpiresAt: ts, SID: "web1.example.com"},
		`{"bootstrap_token":"BOOTSTRAP.TOKEN.PLAIN","expires_at":"2026-06-14T12:34:56.789012345Z","sid":"web1.example.com"}`)

	// --- SoulSshTargetReply: nested class-A SoulSshTarget (ssh_provider задан) ---
	provider := "openssh-prod"
	goldenSoulWire(t, "SshTargetReply/full",
		SoulSshTargetReply{SID: "web1.example.com", SSHTarget: SoulSshTarget{SSHPort: 22, SSHUser: "deploy", SoulPath: "/usr/local/bin/soul", SSHProvider: provider}},
		`{"sid":"web1.example.com","ssh_target":{"soul_path":"/usr/local/bin/soul","ssh_port":22,"ssh_provider":"openssh-prod","ssh_user":"deploy"}}`)
	// --- SoulSshTargetReply: ssh_provider nil (omitempty опущен) ---
	goldenSoulWire(t, "SshTargetReply/nil_provider",
		SoulSshTargetReply{SID: "db1.example.com", SSHTarget: SoulSshTarget{SSHPort: 2222, SSHUser: "root", SoulPath: "/opt/soul"}},
		`{"sid":"db1.example.com","ssh_target":{"soul_path":"/opt/soul","ssh_port":2222,"ssh_user":"root"}}`)

	// --- SoulListEntry: full (все nullable заданы, covens массив) ---
	aid := "archon-alice"
	kid := "keeper-01"
	traits := map[string]any{"tier": "gold"}
	goldenSoulWire(t, "ListEntry/full",
		SoulListEntry{Covens: covens, Traits: traits, CreatedByAID: &aid, LastSeenAt: &ts, LastSeenByKid: &kid, RegisteredAt: ts2, RequestedAt: &ts2, SID: "web1.example.com", Status: SoulStatus("connected"), Transport: SoulTransport("agent")},
		`{"covens":["prod","eu"],"traits":{"tier":"gold"},"created_by_aid":"archon-alice","last_seen_at":"2026-06-14T12:34:56.789012345Z","last_seen_by_kid":"keeper-01","registered_at":"2026-06-13T01:02:03.456789012Z","requested_at":"2026-06-13T01:02:03.456789012Z","sid":"web1.example.com","status":"connected","transport":"agent"}`)
	// --- SoulListEntry: nullable nil → `null` (БЕЗ omitempty), covens/traits пустые (handler
	// coalesce → `[]`/`{}`) ---
	goldenSoulWire(t, "ListEntry/nil_nullables",
		SoulListEntry{Covens: []string{}, Traits: map[string]any{}, CreatedByAID: nil, LastSeenAt: nil, LastSeenByKid: nil, RegisteredAt: ts, RequestedAt: nil, SID: "db1.example.com", Status: SoulStatus("pending"), Transport: SoulTransport("ssh")},
		`{"covens":[],"traits":{},"created_by_aid":null,"last_seen_at":null,"last_seen_by_kid":null,"registered_at":"2026-06-14T12:34:56.789012345Z","requested_at":null,"sid":"db1.example.com","status":"pending","transport":"ssh"}`)

	// --- SoulHistoryReply: scenario-item (incarnation/scenario заданы, module nil) ---
	inc := "web-prod"
	scen := "deploy"
	voy := "voy-123"
	scenItem := SoulHistoryItem{ID: "apply-1", Type: SoulHistoryItemType("scenario"), Status: "succeeded", StartedAt: ts, FinishedAt: &ts2, Incarnation: &inc, Scenario: &scen, VoyageID: &voy}
	// --- errand-item (module задан, scenario/incarnation/finished_at nil — running) ---
	mod := "core.cmd.shell"
	errItem := SoulHistoryItem{ID: "errand-9", Type: SoulHistoryItemType("errand"), Status: "running", StartedAt: ts2, Module: &mod}
	goldenSoulWire(t, "HistoryItem/scenario", scenItem,
		`{"finished_at":"2026-06-13T01:02:03.456789012Z","id":"apply-1","incarnation":"web-prod","scenario":"deploy","started_at":"2026-06-14T12:34:56.789012345Z","status":"succeeded","type":"scenario","voyage_id":"voy-123"}`)
	goldenSoulWire(t, "HistoryItem/errand", errItem,
		`{"id":"errand-9","module":"core.cmd.shell","started_at":"2026-06-13T01:02:03.456789012Z","status":"running","type":"errand"}`)
	goldenSoulWire(t, "HistoryReply/full",
		SoulHistoryReply{Items: []SoulHistoryItem{scenItem, errItem}, Limit: 50, Offset: 0, SID: "web1.example.com", Total: 2},
		`{"items":[{"finished_at":"2026-06-13T01:02:03.456789012Z","id":"apply-1","incarnation":"web-prod","scenario":"deploy","started_at":"2026-06-14T12:34:56.789012345Z","status":"succeeded","type":"scenario","voyage_id":"voy-123"},{"id":"errand-9","module":"core.cmd.shell","started_at":"2026-06-13T01:02:03.456789012Z","status":"running","type":"errand"}],"limit":50,"offset":0,"sid":"web1.example.com","total":2}`)
}

// TestGoldenWire_SoulProjection проверяет, что проекция доменных handlers.Soul*View → native
// сохраняет byte-exact wire против зафиксированного эталона. Ловит регресс в маппинге полей.
func TestGoldenWire_SoulProjection(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 0, 0, 123456789, time.UTC)
	tok := "T"
	covens := []string{"a"}

	createV := handlers.SoulCreateView{BootstrapToken: &tok, Covens: covens, CreatedByAID: "archon-x", ExpiresAt: &ts, RegisteredAt: ts, SID: "h", Status: "connected", Transport: "agent"}
	goldenSoulWire(t, "proj/CreateReply", newSoulCreateReply(createV),
		`{"bootstrap_token":"T","covens":["a"],"created_by_aid":"archon-x","expires_at":"2026-06-14T12:00:00.123456789Z","registered_at":"2026-06-14T12:00:00.123456789Z","sid":"h","status":"connected","transport":"agent"}`)
	// transport=ssh: token/expires nil → ключ опущен; covens пуст (handler даёт coalesce → `[]`).
	emptyCovens := []string{}
	createNilV := handlers.SoulCreateView{BootstrapToken: nil, Covens: emptyCovens, CreatedByAID: "archon-x", ExpiresAt: nil, RegisteredAt: ts, SID: "h", Status: "pending", Transport: "ssh"}
	goldenSoulWire(t, "proj/CreateReply_ssh", newSoulCreateReply(createNilV),
		`{"covens":[],"created_by_aid":"archon-x","registered_at":"2026-06-14T12:00:00.123456789Z","sid":"h","status":"pending","transport":"ssh"}`)

	issueV := handlers.SoulIssueTokenView{BootstrapToken: tok, ExpiresAt: ts, SID: "h"}
	goldenSoulWire(t, "proj/IssueTokenReply", newSoulIssueTokenReply(issueV),
		`{"bootstrap_token":"T","expires_at":"2026-06-14T12:00:00.123456789Z","sid":"h"}`)

	sshV := handlers.SoulSshTargetView{SID: "h", SSHPort: 22, SSHUser: "u", SoulPath: "/s", SSHProvider: "p"}
	goldenSoulWire(t, "proj/SshTargetReply", newSoulSshTargetReply(sshV),
		`{"sid":"h","ssh_target":{"soul_path":"/s","ssh_port":22,"ssh_provider":"p","ssh_user":"u"}}`)
	sshNilV := handlers.SoulSshTargetView{SID: "h", SSHPort: 22, SSHUser: "u", SoulPath: "/s", SSHProvider: ""}
	goldenSoulWire(t, "proj/SshTargetReply_nil", newSoulSshTargetReply(sshNilV),
		`{"sid":"h","ssh_target":{"soul_path":"/s","ssh_port":22,"ssh_user":"u"}}`)

	aid := "archon-x"
	entryV := handlers.SoulListView{Covens: covens, Traits: map[string]any{"tier": "gold"}, CreatedByAID: &aid, LastSeenAt: &ts, LastSeenByKid: &aid, RegisteredAt: ts, RequestedAt: &ts, SID: "h", Status: "connected", Transport: "agent"}
	goldenSoulWire(t, "proj/ListEntry", newSoulListEntry(entryV),
		`{"covens":["a"],"traits":{"tier":"gold"},"created_by_aid":"archon-x","last_seen_at":"2026-06-14T12:00:00.123456789Z","last_seen_by_kid":"archon-x","registered_at":"2026-06-14T12:00:00.123456789Z","requested_at":"2026-06-14T12:00:00.123456789Z","sid":"h","status":"connected","transport":"agent"}`)
	// covens/traits пусты (handler coalesce → `[]`/`{}`); проекция byte-passthrough сохраняет форму.
	entryNilV := handlers.SoulListView{Covens: []string{}, Traits: map[string]any{}, CreatedByAID: nil, LastSeenAt: nil, LastSeenByKid: nil, RegisteredAt: ts, RequestedAt: nil, SID: "h", Status: "pending", Transport: "ssh"}
	goldenSoulWire(t, "proj/ListEntry_nil", newSoulListEntry(entryNilV),
		`{"covens":[],"traits":{},"created_by_aid":null,"last_seen_at":null,"last_seen_by_kid":null,"registered_at":"2026-06-14T12:00:00.123456789Z","requested_at":null,"sid":"h","status":"pending","transport":"ssh"}`)

	inc := "i"
	histV := handlers.SoulHistoryView{Items: []handlers.SoulHistoryItemView{{ID: "a", Type: "scenario", Status: "ok", StartedAt: ts, Incarnation: &inc}}, Limit: 10, Offset: 0, SID: "h", Total: 1}
	goldenSoulWire(t, "proj/HistoryReply", newSoulHistoryReply(histV),
		`{"items":[{"id":"a","incarnation":"i","started_at":"2026-06-14T12:00:00.123456789Z","status":"ok","type":"scenario"}],"limit":10,"offset":0,"sid":"h","total":1}`)
	// nil-Items ветка: handler даёт non-nil [], но проекция обязана сохранить nil → `null`.
	histNilV := handlers.SoulHistoryView{Items: nil, Limit: 0, Offset: 0, SID: "h", Total: 0}
	goldenSoulWire(t, "proj/HistoryReply_nil_items", newSoulHistoryReply(histNilV),
		`{"items":null,"limit":0,"offset":0,"sid":"h","total":0}`)
}
