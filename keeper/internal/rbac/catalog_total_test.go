package rbac

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The [AllowedPermissions] doc comment makes two claims that nothing used to
// check: a TOTAL ("N names") and a per-resource CATEGORY LIST which it says sums
// to that total. Before NIM-728 the two disagreed — the `setting` line was
// missing while the total counted its three — and nothing noticed, because a
// prose claim about a map has no reader.
//
// A test that pinned only the total would not have caught that, and would not
// catch it again: bump the number, forget the line, still green. So this reads
// the comment back and checks BOTH halves against the map it describes.
//
// It reads catalog.go from disk on purpose. The alternative — restating the
// numbers in the test — would just move the same unchecked claim somewhere else.

var (
	// catalogTotalRe matches the "N names (sum of the categories below)" clause.
	catalogTotalRe = regexp.MustCompile(`(?m)^//\s*§Catalog of permissions\.\s*(\d+)\s+names`)
	// catalogCategoryRe matches one category line: "//   - <resource> (N): ...".
	// The resource may be hyphenated (`push-provider`).
	catalogCategoryRe = regexp.MustCompile(`(?m)^//\s+-\s+([a-z-]+)\s+\((\d+)\):`)
)

// TestCatalogDocCommentMatchesTheMap checks the doc comment above
// [AllowedPermissions] against the map: the per-resource counts, the set of
// resources, and the stated total.
func TestCatalogDocCommentMatchesTheMap(t *testing.T) {
	src, err := os.ReadFile("catalog.go")
	if err != nil {
		t.Fatalf("read catalog.go: %v", err)
	}
	doc := string(src)

	// --- what the map actually holds ---
	actual := map[string]int{}
	for name := range AllowedPermissions {
		dot := strings.IndexByte(name, '.')
		if dot < 0 {
			t.Errorf("catalog name %q has no `<resource>.<action>` dot", name)
			continue
		}
		actual[name[:dot]]++
	}

	// --- what the comment claims ---
	claimed := map[string]int{}
	for _, m := range catalogCategoryRe.FindAllStringSubmatch(doc, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("category line for %q carries a non-number: %q", m[1], m[2])
		}
		if _, dup := claimed[m[1]]; dup {
			t.Errorf("the category list names %q twice", m[1])
		}
		claimed[m[1]] = n
	}
	if len(claimed) == 0 {
		t.Fatal("parsed no category lines out of catalog.go — the doc comment's shape changed, " +
			"and with it this test's ability to check anything; fix the regex rather than deleting the test")
	}

	// --- the two must agree, resource by resource ---
	for _, res := range sortedCatalogResources(actual) {
		switch got, ok := claimed[res]; {
		case !ok:
			t.Errorf("resource %q holds %d permissions and appears in NO category line.\n"+
				"That is exactly the drift this test exists for: add the line above "+
				"AllowedPermissions and correct the total.", res, actual[res])
		case got != actual[res]:
			t.Errorf("resource %q: the category list says %d, the map holds %d", res, got, actual[res])
		}
	}
	for _, res := range sortedCatalogResources(claimed) {
		if _, ok := actual[res]; !ok {
			t.Errorf("the category list names %q, which the map does not have", res)
		}
	}

	// --- and the stated total must be the sum, and the map's size ---
	sum := 0
	for _, n := range claimed {
		sum += n
	}
	m := catalogTotalRe.FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("could not find the \"N names (sum of the categories below)\" clause in catalog.go")
	}
	stated, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("stated total is not a number: %q", m[1])
	}
	if stated != sum {
		t.Errorf("the comment states %d names but its own categories sum to %d — "+
			"the claim \"sum of the categories below\" is false", stated, sum)
	}
	if stated != len(AllowedPermissions) {
		t.Errorf("the comment states %d names, the map holds %d", stated, len(AllowedPermissions))
	}
	if t.Failed() {
		t.Log(fmt.Sprintf("map by resource: %v", actual))
	}
}

// sortedCatalogResources gives a deterministic report order.
func sortedCatalogResources(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
