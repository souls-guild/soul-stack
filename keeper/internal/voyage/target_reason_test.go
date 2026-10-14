package voyage

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const voyagesDoc = "../../../docs/keeper/operator-api/voyages.md"

// voyage_targets.error_code has no CHECK, so this table and the doc are the only
// statement of what an operator can find in it. A code written without a doc row
// is a reason nobody can read; a doc row without a code promises one that never
// comes.
func TestTargetErrorCodesMatchDocs(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(voyagesDoc)
	if err != nil {
		t.Fatalf("read %s: %v", voyagesDoc, err)
	}
	_, section, found := strings.Cut(string(raw), "##### Target error codes")
	if !found {
		t.Fatalf("%s has no \"Target error codes\" section", voyagesDoc)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	var documented []string
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		documented = append(documented, m[1])
	}

	inCode := map[string]bool{}
	for _, c := range TargetErrorCodes {
		if inCode[c] {
			t.Errorf("TargetErrorCodes lists %q twice", c)
		}
		inCode[c] = true
	}
	inDoc := map[string]bool{}
	for _, c := range documented {
		if inDoc[c] {
			t.Errorf("%s documents %q twice", voyagesDoc, c)
		}
		inDoc[c] = true
	}
	for c := range inCode {
		if !inDoc[c] {
			t.Errorf("code %q is written to voyage_targets.error_code but not documented in %s", c, voyagesDoc)
		}
	}
	for c := range inDoc {
		if !inCode[c] {
			t.Errorf("%s documents %q, which no code path writes", voyagesDoc, c)
		}
	}
	if t.Failed() {
		sort.Strings(documented)
		t.Logf("documented: %v", documented)
	}
}
