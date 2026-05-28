package policy

import (
	"regexp"
	"testing"
)

func TestIsDefaultBranch(t *testing.T) {
	t.Run("matches-defaults", func(t *testing.T) {
		for _, b := range []string{"main", "master", "develop", "trunk"} {
			if !IsDefaultBranch(b) {
				t.Errorf("IsDefaultBranch(%q) = false; want true", b)
			}
		}
	})
	t.Run("rejects-non-defaults", func(t *testing.T) {
		for _, b := range []string{"feature-x", "release/1.2", "develop-staging", "main-feature", "trunky", "", "Main", "MAIN"} {
			if IsDefaultBranch(b) {
				t.Errorf("IsDefaultBranch(%q) = true; want false", b)
			}
		}
	})
	t.Run("regex-matches-IsDefaultBranch", func(t *testing.T) {
		// Cross-check the exported regex matches what the function reports —
		// catches a future drift where someone changes one but not the other.
		re := regexp.MustCompile(DefaultBranchesRE)
		for _, b := range []string{"main", "master", "develop", "trunk", "feature-x", "release/1.2"} {
			if re.MatchString(b) != IsDefaultBranch(b) {
				t.Errorf("regex/function disagree for %q", b)
			}
		}
	})
}
