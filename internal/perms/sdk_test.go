package perms

import (
	"os"
	"regexp"
	"testing"
)

// The Python and TypeScript test harnesses carry their own copy of the default capability
// table (they run without Luna). This fails when either drifts from minRank.
func TestHarnessTablesMatch(t *testing.T) {
	pair := regexp.MustCompile(`"([a-z_.]+)":\s*"([a-z-]+)"`)
	for _, f := range []string{"../../sdk/python/luna/testing.py", "../../sdk/node/luna/testing.ts"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		block := regexp.MustCompile(`(?s)MIN_RANK[^{]*\{(.*?)\}`).FindSubmatch(src)
		if block == nil {
			t.Fatalf("%s: no MIN_RANK table", f)
		}
		got := map[string]string{}
		for _, m := range pair.FindAllSubmatch(block[1], -1) {
			got[string(m[1])] = string(m[2])
		}
		for c, r := range minRank {
			if r != Broadcaster && got[c] != r.String() { // the harnesses let the broadcaster do anything
				t.Errorf("%s: %s is %q, Luna says %s", f, c, got[c], r)
			}
		}
		for c := range got {
			if _, ok := minRank[c]; !ok {
				t.Errorf("%s: %s isn't a Luna capability", f, c)
			}
		}
	}
}
