package applyrun

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func hostRows(t *testing.T, planIndex int, bySID map[string]map[string]any) []RunPlanHostParams {
	t.Helper()
	var out []RunPlanHostParams
	for _, sid := range sortedSIDKeys(bySID) {
		b, err := json.Marshal(bySID[sid])
		if err != nil {
			t.Fatalf("marshal %s: %v", sid, err)
		}
		out = append(out, RunPlanHostParams{PlanIndex: planIndex, SID: sid, Params: b})
	}
	return out
}

func sortedSIDKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func cellAt(t *testing.T, p *ParamsByHost, path string) ParamCell {
	t.Helper()
	for _, c := range p.Cells {
		if c.Path == path && c.Line == 0 {
			return c
		}
	}
	t.Fatalf("no cell %q in %+v", path, p.Cells)
	return ParamCell{}
}

type variantView struct {
	value  string
	absent bool
	sids   string
}

func variantsOf(c ParamCell) []variantView {
	out := make([]variantView, len(c.Values))
	for i, v := range c.Values {
		val := fmt.Sprint(v.Value)
		if v.Lines != nil {
			val = strings.Join(v.Lines, "\\n")
		}
		out[i] = variantView{value: val, absent: v.Absent, sids: strings.Join(v.SIDs, ",")}
	}
	return out
}

func TestGroupParamsByHost_NoRowsIsNil(t *testing.T) {
	got, err := GroupParamsByHost([]byte(`{"cmd":"echo"}`), nil)
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nil for a task without per-host rows", got, err)
	}
}

// The shape of the B view: a key on the plan row only is one shared value; a key
// in the host rows is per host, grouped by value, the larger group first.
func TestGroupParamsByHost_SharedAndVaryingKeys(t *testing.T) {
	common := mustJSON(t, map[string]any{
		"config_path": "/etc/redis/redis.conf",
		"requirepass": "***MASKED***",
		"maxmemory":   "<per-host>",
		"owner":       "<per-host>",
		"announce_ip": "<per-host>",
	})
	rows := hostRows(t, 0, map[string]map[string]any{
		"redis-01": {"maxmemory": "8gb", "owner": "redis", "announce_ip": "10.20.0.1"},
		"redis-02": {"maxmemory": "4gb", "owner": "redis", "announce_ip": "10.20.0.2"},
		"redis-03": {"maxmemory": "4gb", "owner": "root", "announce_ip": "10.20.0.3"},
	})
	got, err := GroupParamsByHost(common, rows)
	if err != nil {
		t.Fatalf("GroupParamsByHost: %v", err)
	}
	if got.Hosts != 3 {
		t.Errorf("Hosts = %d, want 3", got.Hosts)
	}
	var paths []string
	for _, c := range got.Cells {
		paths = append(paths, c.Path)
	}
	if want := []string{"announce_ip", "config_path", "maxmemory", "owner", "requirepass"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if c := cellAt(t, got, "config_path"); !c.Same || len(c.Values) != 1 || c.Values[0].Value != "/etc/redis/redis.conf" || c.Values[0].SIDs != nil {
		t.Errorf("config_path = %+v, want one shared value without SIDs", c)
	}
	if c := cellAt(t, got, "requirepass"); !c.Same || c.Values[0].Value != "***MASKED***" {
		t.Errorf("requirepass = %+v, want the mask, shared", c)
	}
	if got, want := variantsOf(cellAt(t, got, "maxmemory")), []variantView{{"4gb", false, "redis-02,redis-03"}, {"8gb", false, "redis-01"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("maxmemory = %+v, want %+v", got, want)
	}
	if got, want := variantsOf(cellAt(t, got, "owner")), []variantView{{"redis", false, "redis-01,redis-02"}, {"root", false, "redis-03"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("owner = %+v, want %+v", got, want)
	}
	if got, want := variantsOf(cellAt(t, got, "announce_ip")), []variantView{{"10.20.0.1", false, "redis-01"}, {"10.20.0.2", false, "redis-02"}, {"10.20.0.3", false, "redis-03"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("announce_ip = %+v, want one value per host, by SID", got)
	}
}

// A host without the key is a variant of its own, not a silent omission.
func TestGroupParamsByHost_AbsentKeyIsAVariant(t *testing.T) {
	rows := hostRows(t, 0, map[string]map[string]any{
		"a": {"replicaof": "10.0.0.1:6379"},
		"b": {"replicaof": "10.0.0.1:6379"},
		"c": {},
	})
	got, err := GroupParamsByHost(mustJSON(t, map[string]any{"replicaof": "<per-host>"}), rows)
	if err != nil {
		t.Fatal(err)
	}
	if v, want := variantsOf(cellAt(t, got, "replicaof")), []variantView{{"10.0.0.1:6379", false, "a,b"}, {"<nil>", true, "c"}}; !reflect.DeepEqual(v, want) {
		t.Errorf("replicaof = %+v, want %+v", v, want)
	}
}

// A per-host map is compared leaf by leaf: one differing branch must not hide the
// branches every host agrees on. Lists are ordered by index, not as text.
func TestGroupParamsByHost_NestedChains(t *testing.T) {
	cfg := func(ip string, ports []any) map[string]any {
		return map[string]any{
			"net":         map[string]any{"bind": "0.0.0.0", "port": 6379},
			"replication": map[string]any{"announce_ip": ip},
			"ports":       ports,
			"tls":         map[string]any{},
		}
	}
	ports := make([]any, 11)
	for i := range ports {
		ports[i] = i
	}
	portsB := slices.Clone(ports)
	portsB[10] = 99
	rows := hostRows(t, 0, map[string]map[string]any{
		"a": {"config": cfg("10.0.0.1", ports)},
		"b": {"config": cfg("10.0.0.2", portsB)},
	})
	got, err := GroupParamsByHost(mustJSON(t, map[string]any{"config": "<per-host>"}), rows)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	same := map[string]bool{}
	for _, c := range got.Cells {
		order = append(order, c.Path)
		same[c.Path] = c.Same
	}
	if i2, i10 := slices.Index(order, "config.ports[2]"), slices.Index(order, "config.ports[10]"); i2 < 0 || i10 < 0 || i2 > i10 {
		t.Errorf("order = %v, want config.ports[2] before config.ports[10]", order)
	}
	for _, p := range []string{"config.net.bind", "config.net.port", "config.ports[0]", "config.tls"} {
		if !same[p] {
			t.Errorf("%s: Same = false, want a leaf both hosts share listed as shared", p)
		}
	}
	for _, p := range []string{"config.replication.announce_ip", "config.ports[10]"} {
		if same[p] {
			t.Errorf("%s: Same = true, want the differing leaf", p)
		}
	}
	if c := cellAt(t, got, "config.tls"); fmt.Sprint(c.Values[0].Value) != "map[]" {
		t.Errorf("config.tls = %+v, want the empty map as a leaf", c)
	}
}

// Numbers are shown as stored: a 20-digit integer survives the read path.
func TestGroupParamsByHost_KeepsLargeIntegers(t *testing.T) {
	rows := []RunPlanHostParams{
		{SID: "a", Params: []byte(`{"id":12345678901234567890}`)},
		{SID: "b", Params: []byte(`{"id":1}`)},
	}
	got, err := GroupParamsByHost([]byte(`{"id":"<per-host>"}`), rows)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cellAt(t, got, "id").Values)
	if !strings.Contains(string(b), "12345678901234567890") {
		t.Errorf("values = %s, want the integer unrounded", b)
	}
}

func TestGroupParamsByHost_UndecodableRowIsAnError(t *testing.T) {
	_, err := GroupParamsByHost([]byte(`{"x":"<per-host>"}`), []RunPlanHostParams{{SID: "a", Params: []byte(`{`)}})
	if err == nil {
		t.Fatal("want an error for a row that does not decode")
	}
}

// A long single-line string is a value, and so is a text some hosts lack: line
// numbers need a text on every host.
func TestGroupParamsByHost_TextModeNeedsMultiLineOnEveryHost(t *testing.T) {
	long := strings.Repeat("x", 5000)
	rows := hostRows(t, 0, map[string]map[string]any{
		"a": {"one": long, "partial": "l1\nl2"},
		"b": {"one": long + "y"},
	})
	got, err := GroupParamsByHost(mustJSON(t, map[string]any{"one": "<per-host>", "partial": "<per-host>"}), rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 0 {
		t.Errorf("Texts = %+v, want none", got.Texts)
	}
	for _, c := range got.Cells {
		if c.Line != 0 {
			t.Errorf("cell %+v is a hunk, want whole-value cells", c)
		}
	}
}

// The redis.conf of mockup H: one line differs on every host, one on the masters,
// and the masters lack a third. The base is a replica's text, so the masters read
// "no line" at 6 instead of shifting every replica's numbers. Lines the hosts
// share keep the hunks apart; adjacent changed lines form one hunk.
func TestGroupParamsByHost_TextHunks(t *testing.T) {
	conf := func(ip, mem string, replicaOf bool) string {
		lines := []string{"port 6379", "announce-ip " + ip, "# memory", "maxmemory " + mem, "# topology"}
		if replicaOf {
			lines = append(lines, "replicaof 10.0.0.1 6379")
		}
		return strings.Join(append(lines, "appendonly yes"), "\n")
	}
	texts := map[string]string{
		"m1": conf("10.0.0.1", "8gb", false),
		"r1": conf("10.0.0.2", "4gb", true),
		"r2": conf("10.0.0.3", "4gb", true),
		"r3": conf("10.0.0.4", "4gb", true),
	}
	rowsIn := map[string]map[string]any{}
	for sid, s := range texts {
		rowsIn[sid] = map[string]any{"content": s}
	}
	got, err := GroupParamsByHost(mustJSON(t, map[string]any{"content": "<per-host>", "path": "/etc/redis/redis.conf"}), hostRows(t, 0, rowsIn))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 1 || got.Texts[0].Path != "content" || len(got.Texts[0].Lines) != 7 {
		t.Fatalf("Texts = %+v, want the 7-line replica text as base", got.Texts)
	}
	type hunkView struct {
		line, count int
		variants    []variantView
	}
	var hunks []hunkView
	for _, c := range got.Cells {
		if c.Path == "content" {
			hunks = append(hunks, hunkView{c.Line, c.LineCount, variantsOf(c)})
		}
	}
	want := []hunkView{
		{2, 1, []variantView{
			{"announce-ip 10.0.0.1", false, "m1"},
			{"announce-ip 10.0.0.2", false, "r1"},
			{"announce-ip 10.0.0.3", false, "r2"},
			{"announce-ip 10.0.0.4", false, "r3"},
		}},
		{4, 1, []variantView{
			{"maxmemory 4gb", false, "r1,r2,r3"},
			{"maxmemory 8gb", false, "m1"},
		}},
		{6, 1, []variantView{
			{"replicaof 10.0.0.1 6379", false, "r1,r2,r3"},
			{"<nil>", true, "m1"},
		}},
	}
	if !reflect.DeepEqual(hunks, want) {
		t.Errorf("hunks =\n%+v\nwant\n%+v", hunks, want)
	}
	for sid, s := range texts {
		if r := strings.Join(reconstructText(got, "content", sid), "\n"); r != s {
			t.Errorf("%s rebuilt as %q, want %q", sid, r, s)
		}
	}
}

// ★ The contract the UI's "host text" relies on: for any set of texts, the base
// with each hunk's range replaced by a host's variant IS that host's text, the
// hunks are disjoint and ascending, and each lists every host exactly once.
// Mutation: start a new hunk for every change instead of merging overlaps → a
// change spanning two hunks rebuilds wrong text (or panics on the slice).
func TestGroupParamsByHost_TextHunksRebuildEveryHost(t *testing.T) {
	rng := rand.New(rand.NewSource(985))
	words := []string{"a", "b", "c", "", "port 1", "port 2", "x"}
	for iter := 0; iter < 400; iter++ {
		base := make([]string, 2+rng.Intn(12))
		for i := range base {
			base[i] = words[rng.Intn(len(words))]
		}
		texts := map[string]string{}
		hosts := 2 + rng.Intn(5)
		for h := 0; h < hosts; h++ {
			lines := slices.Clone(base)
			for e := rng.Intn(4); e > 0; e-- {
				switch pos := rng.Intn(len(lines) + 1); rng.Intn(3) {
				case 0:
					lines = slices.Insert(lines, pos, words[rng.Intn(len(words))])
				case 1:
					if pos < len(lines) && len(lines) > 1 {
						lines = slices.Delete(lines, pos, pos+1)
					}
				default:
					if pos < len(lines) {
						lines[pos] = words[rng.Intn(len(words))] + "!"
					}
				}
			}
			texts[fmt.Sprintf("h%02d", h)] = strings.Join(append(lines, "end"), "\n")
		}
		rowsIn := map[string]map[string]any{}
		for sid, s := range texts {
			rowsIn[sid] = map[string]any{"t": s}
		}
		got, err := GroupParamsByHost([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, rowsIn))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Texts) == 0 {
			continue // every host drew the same text
		}
		prevEnd := -1
		for _, c := range got.Cells {
			if c.Line == 0 {
				continue
			}
			if c.Line-1 < prevEnd {
				t.Fatalf("iter %d: hunk at %d overlaps the previous one ending at %d", iter, c.Line, prevEnd)
			}
			prevEnd = c.Line - 1 + c.LineCount
			var seen []string
			for _, v := range c.Values {
				seen = append(seen, v.SIDs...)
			}
			slices.Sort(seen)
			if !reflect.DeepEqual(seen, sortedSIDKeys(texts)) {
				t.Fatalf("iter %d: hunk at %d lists %v, want every host once", iter, c.Line, seen)
			}
		}
		for sid, s := range texts {
			if r := strings.Join(reconstructText(got, "t", sid), "\n"); r != s {
				t.Fatalf("iter %d: %s rebuilt as %q, want %q", iter, sid, r, s)
			}
		}
	}
}

func reconstructText(p *ParamsByHost, path, sid string) []string {
	var base []string
	for _, tx := range p.Texts {
		if tx.Path == path {
			base = tx.Lines
		}
	}
	var out []string
	pos := 0
	for _, c := range p.Cells {
		if c.Path != path || c.Line == 0 {
			continue
		}
		start := c.Line - 1
		out = append(out, base[pos:start]...)
		for _, v := range c.Values {
			if slices.Contains(v.SIDs, sid) {
				out = append(out, v.Lines...)
			}
		}
		pos = start + c.LineCount
	}
	return append(out, base[pos:]...)
}

// A key holding a dot is not the chain it reads like: the two leaves keep two
// paths, so the reply never carries two cells under one name.
func TestGroupParamsByHost_PathsStayDistinctForDottedKeys(t *testing.T) {
	rows := hostRows(t, 0, map[string]map[string]any{
		"a": {"a.b": "x-a", "a": map[string]any{"b": "y-a"}},
		"b": {"a.b": "x-b", "a": map[string]any{"b": "y-b"}},
	})
	got, err := GroupParamsByHost(mustJSON(t, map[string]any{"a.b": "<per-host>", "a": "<per-host>"}), rows)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range got.Cells {
		paths = append(paths, c.Path)
	}
	if want := []string{"a.b", `["a.b"]`}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

// Past the diff bound a text is compared whole: per host, and over all hosts.
func TestGroupParamsByHost_TextPastTheBoundIsAWholeValue(t *testing.T) {
	long := strings.Repeat("l\n", maxDiffLines)
	for _, tc := range []struct {
		name  string
		texts map[string]string
	}{
		{"one host past maxDiffLines", map[string]string{"a": long + "a", "b": "b\nb"}},
		{"hosts past maxDiffLinesTotal", func() map[string]string {
			m := map[string]string{}
			for i := 0; i <= maxDiffLinesTotal/maxDiffLines; i++ {
				m[fmt.Sprintf("h%03d", i)] = strings.Repeat("l\n", maxDiffLines-1) + fmt.Sprint(i)
			}
			return m
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]map[string]any{}
			for sid, s := range tc.texts {
				in[sid] = map[string]any{"t": s}
			}
			got, err := GroupParamsByHost([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, in))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Texts) != 0 || len(got.Cells) != 1 || got.Cells[0].Line != 0 {
				t.Errorf("cells = %d, texts = %d, want one whole-value cell", len(got.Cells), len(got.Texts))
			}
		})
	}
}

// ★ The diff budget is one pool: shared by the hosts of a leaf, and by every task
// of one read. A host whose diff fits alone does not fit after the others spent it.
// Mutations: a fresh budget per host → four hosts fit where two should; a fresh
// budget per task (GroupParamsByHost in the read loop) → the second task diffs.
func TestGroupParamsByHostWithin_BudgetIsShared(t *testing.T) {
	base := make([]string, 400)
	for i := range base {
		base[i] = fmt.Sprintf("line %d", i)
	}
	texts := func(hosts int) map[string]map[string]any {
		in := map[string]map[string]any{
			"base-1": {"t": strings.Join(base, "\n")},
			"base-2": {"t": strings.Join(base, "\n")},
		}
		for h := 0; h < hosts; h++ {
			own := slices.Clone(base)
			own[50+h*80] = fmt.Sprintf("own %d", h)
			in[fmt.Sprintf("h%d", h)] = map[string]any{"t": strings.Join(own, "\n")}
		}
		return in
	}
	hunked := func(p *ParamsByHost) bool { return len(p.Texts) == 1 }

	one := 1 << 40
	got, err := GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, texts(1)), &one)
	if err != nil || !hunked(got) {
		t.Fatalf("one host: %+v, %v; want hunks", got, err)
	}
	perHost := 1<<40 - one

	budget := perHost*2 + perHost/2
	got, _ = GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, texts(4)), &budget)
	if hunked(got) {
		t.Errorf("four hosts at %d steps each fit a budget of %d - the hosts do not share it", perHost, perHost*2+perHost/2)
	}

	budget = perHost + perHost/2
	first, _ := GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, texts(1)), &budget)
	second, _ := GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 1, texts(1)), &budget)
	if !hunked(first) || hunked(second) {
		t.Errorf("first hunked = %v, second hunked = %v; want the second task to find the budget spent", hunked(first), hunked(second))
	}
}

// ★ One leaf draws at most diffBudget from the read's budget, however much is
// left: nine hosts 498 edits from the base fit (~3.4M steps), eleven do not
// (~4.2M), with the whole RequestDiffBudget available.
// Mutation: take the leaf's share as all of the read's budget → eleven fit.
func TestGroupParamsByHostWithin_LeafIsCappedByDiffBudget(t *testing.T) {
	base := make([]string, 5000)
	for i := range base {
		base[i] = fmt.Sprintf("line %d", i)
	}
	texts := func(hosts int) map[string]map[string]any {
		in := map[string]map[string]any{
			"base-1": {"t": strings.Join(base, "\n")},
			"base-2": {"t": strings.Join(base, "\n")},
		}
		for h := 0; h < hosts; h++ {
			own := slices.Clone(base)
			for c := 0; c < 249; c++ {
				own[c*20+h] = fmt.Sprintf("own %d %d", h, c)
			}
			in[fmt.Sprintf("h%02d", h)] = map[string]any{"t": strings.Join(own, "\n")}
		}
		return in
	}
	for _, tc := range []struct {
		hosts  int
		hunked bool
	}{{9, true}, {11, false}} {
		budget := RequestDiffBudget
		got, err := GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, texts(tc.hosts)), &budget)
		if err != nil {
			t.Fatal(err)
		}
		if hunked := len(got.Texts) == 1; hunked != tc.hunked {
			t.Errorf("%d hosts: split into hunks = %v, want %v", tc.hosts, hunked, tc.hunked)
		}
		if spent := RequestDiffBudget - budget; spent > diffBudget+maxDiffLines+2*maxDiffEdits+3 {
			t.Errorf("%d hosts: the leaf spent %d steps, past its share of %d", tc.hosts, spent, diffBudget)
		}
	}
}

// ★ Building the hunks draws on the budget too, and a leaf that runs out there
// appends nothing of its text: one whole-value cell, no Texts entry.
// Mutations: drop the hunk-build charge → the leaf is split; append the Texts
// entry before building the hunks → a Texts entry ships beside a whole value.
func TestGroupParamsByHostWithin_HunkBuildIsCharged(t *testing.T) {
	in := map[string]map[string]any{
		"base-1": {"t": "a\nb\nc"},
		"base-2": {"t": "a\nb\nc"},
		"h":      {"t": "a\nx\nc"},
	}
	diffOnly := 1 << 30
	if _, ok := diffLines(ids("abc"), ids("axc"), maxDiffEdits, &diffOnly); !ok {
		t.Fatal("the diff gave up")
	}
	budget := (1<<30 - diffOnly) + 1
	got, err := GroupParamsByHostWithin([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, in), &budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 0 || len(got.Cells) != 1 || got.Cells[0].Line != 0 {
		t.Errorf("texts = %d, cells = %+v; want one whole-value cell and no Texts entry", len(got.Texts), got.Cells)
	}
}

// A value nested past maxPathDepth is one leaf at that depth, so a deep param
// costs a bounded chain per leaf.
func TestGroupParamsByHost_DeepValueIsCutAtMaxPathDepth(t *testing.T) {
	deep := func(leaf string) map[string]any {
		var v any = leaf
		for i := 0; i < maxPathDepth+10; i++ {
			v = map[string]any{"n": v}
		}
		return map[string]any{"d": v}
	}
	got, err := GroupParamsByHost([]byte(`{"d":"<per-host>"}`), hostRows(t, 0, map[string]map[string]any{
		"a": deep("x"), "b": deep("y"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cells) != 1 {
		t.Fatalf("cells = %d, want one", len(got.Cells))
	}
	if segs := strings.Count(got.Cells[0].Path, ".") + 1; segs != maxPathDepth {
		t.Errorf("path has %d segments, want %d", segs, maxPathDepth)
	}
	if _, isMap := got.Cells[0].Values[0].Value.(map[string]any); !isMap {
		t.Errorf("value = %T, want the remaining subtree", got.Cells[0].Values[0].Value)
	}
}
