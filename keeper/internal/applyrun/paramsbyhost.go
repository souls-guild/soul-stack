package applyrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ParamsByHost is one task's params seen across the hosts it was rendered for
// (NIM-985): every leaf with the values it took and the hosts that took each.
// It is built from the plan row and the task's per-host rows, both masked on the
// write path, so it can show nothing the plan row could not.
type ParamsByHost struct {
	// Hosts is how many hosts the task was rendered for.
	Hosts int

	// Cells are in path order; the hunks of one text follow each other in line
	// order under its path.
	Cells []ParamCell

	// Texts holds the base text of every leaf split into hunks. Replacing each of
	// its hunks' line ranges by a host's variant gives that host's text.
	Texts []ParamText
}

// ParamCell is one leaf of the params tree, or one hunk of a multi-line leaf.
type ParamCell struct {
	// Path is the leaf's chain: `config.replication.role`, `acl.users[0].name`,
	// `labels["app.kubernetes.io/name"]`.
	Path string

	// Line is the 1-based base-text line a hunk starts at; 0 for a cell that is
	// not a hunk.
	Line int

	// LineCount is how many base-text lines the hunk replaces; 0 is an insertion
	// before Line.
	LineCount int

	// Same marks a leaf equal on every host: Values holds one entry, without SIDs.
	Same bool

	// Values are ordered by host count, largest first, then by first SID.
	Values []ParamVariant
}

// ParamVariant is one value of a cell and the hosts that received it.
type ParamVariant struct {
	Value any

	// Absent: the host has no such leaf, or a hunk has no lines on it.
	Absent bool

	// Lines is a hunk variant's lines.
	Lines []string

	SIDs []string
}

// ParamText is the base text a path's hunks are line ranges of.
type ParamText struct {
	Path  string
	Lines []string
}

// The line diff runs on every read of /tasks, which the UI polls, so its cost is
// bounded and a leaf past any bound is compared as a whole value instead: a text
// over maxDiffLines, hosts' texts adding up to over maxDiffLinesTotal, a host
// more than maxDiffEdits edits from the base (a changed line is two: one removed,
// one added), or a leaf that would spend more than diffBudget steps — or more
// than the request has left of RequestDiffBudget. A step is one line compared or
// one trace slot allocated.
const (
	maxDiffLines      = 5000
	maxDiffLinesTotal = 200000
	maxDiffEdits      = 500
	diffBudget        = 4000000

	// RequestDiffBudget is what one read of a run's tasks may spend on line
	// diffs across all of them; see [GroupParamsByHostWithin].
	RequestDiffBudget = 16000000

	// maxPathDepth cuts a leaf's chain: a subtree deeper than this is one leaf,
	// so a deeply nested value costs a bounded amount per leaf on every read.
	maxPathDepth = 32
)

// GroupParamsByHost is [GroupParamsByHostWithin] with a budget of its own.
func GroupParamsByHost(common []byte, rows []RunPlanHostParams) (*ParamsByHost, error) {
	budget := RequestDiffBudget
	return GroupParamsByHostWithin(common, rows, &budget)
}

// GroupParamsByHostWithin builds the per-host view of one task from its plan-row
// params and its per-host rows. A key present in any host row is per-host; every
// other plan-row key is shared and listed once. No rows → nil. budget is shared
// by every task of one read: the line diffs draw on it, and once it runs out the
// remaining texts are compared whole.
func GroupParamsByHostWithin(common []byte, rows []RunPlanHostParams, budget *int) (*ParamsByHost, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	shared, err := decodeParams(common)
	if err != nil {
		return nil, fmt.Errorf("applyrun: plan params: %w", err)
	}

	sids := make([]string, 0, len(rows))
	byHost := make(map[string]map[string]any, len(rows))
	perHost := map[string]bool{}
	for _, r := range rows {
		m, err := decodeParams(r.Params)
		if err != nil {
			return nil, fmt.Errorf("applyrun: host %s params: %w", r.SID, err)
		}
		if _, dup := byHost[r.SID]; !dup {
			sids = append(sids, r.SID)
		}
		byHost[r.SID] = m
		for k := range m {
			perHost[k] = true
		}
	}
	sort.Strings(sids)

	leaves := map[string]*paramLeaf{}
	leafAt := func(c paramChain) *paramLeaf {
		id := c.id()
		l := leaves[id]
		if l == nil {
			l = &paramLeaf{chain: c, vals: map[string]any{}}
			leaves[id] = l
		}
		return l
	}
	for k, v := range shared {
		if perHost[k] {
			continue
		}
		flattenParams(paramChain{{key: k}}, v, func(c paramChain, v any) {
			l := leafAt(c)
			l.shared, l.value = true, v
		})
	}
	for _, sid := range sids {
		for k, v := range byHost[sid] {
			flattenParams(paramChain{{key: k}}, v, func(c paramChain, v any) {
				leafAt(c).vals[sid] = v
			})
		}
	}

	ordered := make([]*paramLeaf, 0, len(leaves))
	for _, l := range leaves {
		ordered = append(ordered, l)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].chain.less(ordered[j].chain) })

	out := &ParamsByHost{Hosts: len(sids)}
	for _, l := range ordered {
		if l.shared {
			out.Cells = append(out.Cells, ParamCell{Path: l.chain.String(), Same: true, Values: []ParamVariant{{Value: l.value}}})
			continue
		}
		out.appendLeaf(l, sids, budget)
	}
	return out, nil
}

type paramLeaf struct {
	chain  paramChain
	shared bool
	value  any
	vals   map[string]any
}

func (p *ParamsByHost) appendLeaf(l *paramLeaf, sids []string, budget *int) {
	path := l.chain.String()
	everyHost, equal := true, true
	var first any
	seen := false
	for _, sid := range sids {
		v, ok := l.vals[sid]
		if !ok {
			everyHost, equal = false, false
			continue
		}
		if !seen {
			first, seen = v, true
		} else if !reflect.DeepEqual(first, v) {
			equal = false
		}
	}
	if everyHost && equal {
		p.Cells = append(p.Cells, ParamCell{Path: path, Same: true, Values: []ParamVariant{{Value: first}}})
		return
	}
	if everyHost && *budget > 0 {
		if texts, ok := textLeaf(l, sids); ok && p.appendTextHunks(path, texts, sids, budget) {
			return
		}
	}
	p.Cells = append(p.Cells, ParamCell{Path: path, Values: groupLeafValues(l, sids)})
}

// textLeaf reports whether a leaf is compared line by line: a string on every
// host, multi-line on at least one, and within the diff bounds.
func textLeaf(l *paramLeaf, sids []string) (map[string][]string, bool) {
	out := make(map[string][]string, len(sids))
	multiLine := false
	total := 0
	for _, sid := range sids {
		s, ok := l.vals[sid].(string)
		if !ok {
			return nil, false
		}
		count := strings.Count(s, "\n") + 1
		total += count
		if count > maxDiffLines || total > maxDiffLinesTotal {
			return nil, false
		}
		lines := strings.Split(s, "\n")
		if len(lines) > 1 {
			multiLine = true
		}
		out[sid] = lines
	}
	return out, multiLine
}

func groupLeafValues(l *paramLeaf, sids []string) []ParamVariant {
	const absentKey = "\x00absent"
	byKey := map[string]int{}
	var out []ParamVariant
	for _, sid := range sids {
		v, ok := l.vals[sid]
		key := absentKey
		if ok {
			key = canonicalValue(v)
		}
		i, found := byKey[key]
		if !found {
			i = len(out)
			byKey[key] = i
			out = append(out, ParamVariant{Value: v, Absent: !ok})
		}
		out[i].SIDs = append(out[i].SIDs, sid)
	}
	sortVariants(out)
	return out
}

// appendTextHunks diffs every host's text against one base text and lists the
// base line ranges where any host differs. Ranges that touch or overlap across
// hosts merge into one hunk, so every host's change falls inside exactly one
// hunk and each hunk carries every host's lines for that range. It appends
// nothing and reports false when the diffs, or building the hunks — a step per
// host per hunk plus one per line it carries — run out of their bounds; the steps
// spent are taken from budget either way.
func (p *ParamsByHost) appendTextHunks(path string, texts map[string][]string, sids []string, budget *int) bool {
	base := textBase(texts, sids)

	ids := map[string]int{}
	intern := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, line := range lines {
			id, ok := ids[line]
			if !ok {
				id = len(ids)
				ids[line] = id
			}
			out[i] = id
		}
		return out
	}
	baseIDs := intern(base)

	type change struct {
		s, e, hs, he int
		sid          string
		hunk         int
	}
	var changes []change
	leaf := min(diffBudget, *budget)
	spend := leaf
	defer func() { *budget -= leaf - spend }()
	for _, sid := range sids {
		lines := texts[sid]
		if reflect.DeepEqual(lines, base) {
			continue
		}
		edits, ok := diffLines(baseIDs, intern(lines), maxDiffEdits, &spend)
		if !ok {
			return false
		}
		for _, ed := range edits {
			changes = append(changes, change{s: ed.s, e: ed.e, hs: ed.hs, he: ed.he, sid: sid})
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].s != changes[j].s {
			return changes[i].s < changes[j].s
		}
		return changes[i].e < changes[j].e
	})

	type hunk struct{ s, e int }
	var hunks []hunk
	for i := range changes {
		c := &changes[i]
		if n := len(hunks); n > 0 && c.s <= hunks[n-1].e {
			if c.e > hunks[n-1].e {
				hunks[n-1].e = c.e
			}
		} else {
			hunks = append(hunks, hunk{s: c.s, e: c.e})
		}
		c.hunk = len(hunks) - 1
	}

	changesOf := make([]map[string][]change, len(hunks))
	for _, c := range changes {
		if changesOf[c.hunk] == nil {
			changesOf[c.hunk] = map[string][]change{}
		}
		changesOf[c.hunk][c.sid] = append(changesOf[c.hunk][c.sid], c)
	}

	cells := make([]ParamCell, 0, len(hunks))
	for hi, h := range hunks {
		byKey := map[string]int{}
		var variants []ParamVariant
		for _, sid := range sids {
			lines := texts[sid]
			var rep []string
			pos := h.s
			for _, c := range changesOf[hi][sid] {
				rep = append(rep, base[pos:c.s]...)
				rep = append(rep, lines[c.hs:c.he]...)
				pos = c.e
			}
			rep = append(rep, base[pos:h.e]...)
			spend -= len(rep) + 1
			if spend < 0 {
				return false
			}

			key := canonicalValue(rep)
			i, found := byKey[key]
			if !found {
				i = len(variants)
				byKey[key] = i
				variants = append(variants, ParamVariant{Lines: rep, Absent: len(rep) == 0})
			}
			variants[i].SIDs = append(variants[i].SIDs, sid)
		}
		sortVariants(variants)
		cells = append(cells, ParamCell{Path: path, Line: h.s + 1, LineCount: h.e - h.s, Values: variants})
	}
	p.Texts = append(p.Texts, ParamText{Path: path, Lines: base})
	p.Cells = append(p.Cells, cells...)
	return true
}

// textBase picks the text the hunks are numbered against: the one most hosts
// received; on a tie, the one whose line count most hosts share, so a host that
// lacks a line shows as "no line" instead of shifting everyone else's numbers;
// then the first by SID.
func textBase(texts map[string][]string, sids []string) []string {
	count := map[string]int{}
	byLen := map[int]int{}
	for _, sid := range sids {
		count[strings.Join(texts[sid], "\n")]++
		byLen[len(texts[sid])]++
	}
	var best []string
	bestCount, bestLen := -1, -1
	seen := map[string]bool{}
	for _, sid := range sids {
		lines := texts[sid]
		key := strings.Join(lines, "\n")
		if seen[key] {
			continue
		}
		seen[key] = true
		c, l := count[key], byLen[len(lines)]
		if c > bestCount || (c == bestCount && l > bestLen) {
			best, bestCount, bestLen = lines, c, l
		}
	}
	return best
}

func sortVariants(vs []ParamVariant) {
	sort.SliceStable(vs, func(i, j int) bool {
		if len(vs[i].SIDs) != len(vs[j].SIDs) {
			return len(vs[i].SIDs) > len(vs[j].SIDs)
		}
		return vs[i].SIDs[0] < vs[j].SIDs[0]
	})
}

func canonicalValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(b)
}

// decodeParams keeps numbers as json.Number, so a large integer is shown as
// stored rather than rounded through float64.
func decodeParams(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

type paramSegment struct {
	key   string
	index int
	isIdx bool
}

type paramChain []paramSegment

// String writes a key that a chain separator could misread — empty, or holding a
// dot, a bracket or a quote — as `["…"]`, so two different chains never print
// the same.
func (c paramChain) String() string {
	var b strings.Builder
	for i, s := range c {
		switch {
		case s.isIdx:
			b.WriteString("[" + strconv.Itoa(s.index) + "]")
		case s.key == "" || strings.ContainsAny(s.key, `.[]"`):
			b.WriteString("[" + strconv.Quote(s.key) + "]")
		default:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.key)
		}
	}
	return b.String()
}

func (c paramChain) id() string {
	var b strings.Builder
	for _, s := range c {
		if s.isIdx {
			b.WriteString("\x00i" + strconv.Itoa(s.index))
		} else {
			b.WriteString("\x00k" + s.key)
		}
	}
	return b.String()
}

// less orders map keys by name and list elements by index, so `x[2]` comes
// before `x[10]`.
func (c paramChain) less(o paramChain) bool {
	for i := 0; i < len(c) && i < len(o); i++ {
		a, b := c[i], o[i]
		if a.isIdx != b.isIdx {
			return a.isIdx
		}
		if a.isIdx {
			if a.index != b.index {
				return a.index < b.index
			}
			continue
		}
		if a.key != b.key {
			return a.key < b.key
		}
	}
	return len(c) < len(o)
}

// flattenParams walks v under the chain c and emits every leaf with its own
// copy of the chain, a subtree past maxPathDepth as one leaf. The walk shares
// one stack, so a chain is copied once, at its leaf.
func flattenParams(c paramChain, v any, emit func(paramChain, any)) {
	stack := append(make(paramChain, 0, len(c)+8), c...)
	var walk func(v any)
	walk = func(v any) {
		leaf := func() { emit(append(paramChain(nil), stack...), v) }
		if len(stack) >= maxPathDepth {
			leaf()
			return
		}
		switch x := v.(type) {
		case map[string]any:
			if len(x) == 0 {
				leaf()
				return
			}
			for k, vv := range x {
				stack = append(stack, paramSegment{key: k})
				walk(vv)
				stack = stack[:len(stack)-1]
			}
		case []any:
			if len(x) == 0 {
				leaf()
				return
			}
			for i, vv := range x {
				stack = append(stack, paramSegment{index: i, isIdx: true})
				walk(vv)
				stack = stack[:len(stack)-1]
			}
		default:
			leaf()
		}
	}
	walk(v)
}
