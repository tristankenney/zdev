package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestParseInitiativeMD(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want initiativeDoc
	}{
		{
			name: "empty file — everything absent, never an error",
			in:   "",
			want: initiativeDoc{},
		},
		{
			name: "flat template fields",
			in: "# x\n\n**Intent:** ship the thing\n**Started:** 2026-07-30\n**Tracker:** TBD\n\n" +
				"## Decisions\n\n## Outcome\n<!-- filled at wind-down -->\n",
			want: initiativeDoc{
				Intent:  strPtr("ship the thing"),
				Started: strPtr("2026-07-30"),
				Tracker: strPtr("TBD"),
			},
		},
		{
			name: "intent wraps until the next bold field",
			in: "# x\n\n**Intent:** Ship the MVP — a directory\nwhere customers find suppliers,\n" +
				"so payments land on Pay.\n**Started:** 2026-07-30\n",
			want: initiativeDoc{
				Intent:  strPtr("Ship the MVP — a directory where customers find suppliers, so payments land on Pay."),
				Started: strPtr("2026-07-30"),
			},
		},
		{
			name: "intent wrap stops at a blank line",
			in:   "**Intent:** first line\ncontinues here\n\nthis is body prose, not intent\n",
			want: initiativeDoc{Intent: strPtr("first line continues here")},
		},
		{
			name: "decisions: em-dash and hyphen dates, continuation lines join with spaces",
			in: "## Decisions\n\n- 2026-07-30 — chose the in-app build\n  because the trigger was\n" +
				"  unauthenticated access.\n- 2026-07-31 - hyphen separator works too\n",
			want: initiativeDoc{Decisions: []decision{
				{Date: "2026-07-30", Text: "chose the in-app build because the trigger was unauthenticated access."},
				{Date: "2026-07-31", Text: "hyphen separator works too"},
			}},
		},
		{
			name: "decisions: nested sub-bullets are continuations; undated bullets are not decisions",
			in: "## Decisions\n- 2026-08-03 — telemetry via Intune.\n  - **Not a measurement yet.** exports drop\n" +
				"    silently.\n- just a note without a date\n  its continuation attaches to nothing\n",
			want: initiativeDoc{Decisions: []decision{
				{Date: "2026-08-03", Text: "telemetry via Intune. - **Not a measurement yet.** exports drop silently."},
			}},
		},
		{
			name: "decisions stop at the next section heading",
			in:   "## Decisions\n- 2026-07-30 — one\n\n## Outcome\n- 2026-07-31 — not a decision, wrong section\n",
			want: initiativeDoc{
				Decisions: []decision{{Date: "2026-07-30", Text: "one"}},
				Outcome:   strPtr("- 2026-07-31 — not a decision, wrong section"),
			},
		},
		{
			name: "outcome: comment-only body is null",
			in:   "## Outcome\n<!-- filled at wind-down: what landed, where -->\n",
			want: initiativeDoc{},
		},
		{
			name: "outcome: real text survives comment stripping",
			in:   "## Outcome\n<!-- template -->\nShipped phase 1; phase 2 abandoned\n<!-- trailing -->\n",
			want: initiativeDoc{Outcome: strPtr("Shipped phase 1; phase 2 abandoned")},
		},
		{
			name: "missing sections — no decisions heading means zero decisions",
			in:   "**Intent:** x\n\nSome prose with - 2026-07-30 — a date outside any section\n",
			want: initiativeDoc{Intent: strPtr("x")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInitiativeMD([]byte(tc.in))
			assertStrPtr(t, "intent", got.Intent, tc.want.Intent)
			assertStrPtr(t, "started", got.Started, tc.want.Started)
			assertStrPtr(t, "tracker", got.Tracker, tc.want.Tracker)
			assertStrPtr(t, "outcome", got.Outcome, tc.want.Outcome)
			if len(got.Decisions) != len(tc.want.Decisions) {
				t.Fatalf("decisions = %+v; want %+v", got.Decisions, tc.want.Decisions)
			}
			for i := range got.Decisions {
				if got.Decisions[i] != tc.want.Decisions[i] {
					t.Errorf("decision[%d] = %+v; want %+v", i, got.Decisions[i], tc.want.Decisions[i])
				}
			}
		})
	}
}

func assertStrPtr(t *testing.T, field string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v; want %v", field, fmtStrPtr(got), fmtStrPtr(want))
	case *got != *want:
		t.Errorf("%s = %q; want %q", field, *got, *want)
	}
}

func fmtStrPtr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *p)
}

func TestLatestDecisions(t *testing.T) {
	all := []decision{
		{Date: "2026-07-30", Text: "a"},
		{Date: "2026-08-03", Text: "b"},
		{Date: "2026-07-30", Text: "c"},
		{Date: "2026-07-31", Text: "d"},
		{Date: "2026-08-03", Text: "e"},
		{Date: "2026-07-29", Text: "f"},
	}
	got := latestDecisions(all, 5)
	// 5 most recent by date, ties keep file order, oldest-first in the slice:
	// the 2026-07-29 entry falls off; the two 07-30s keep a-before-c.
	want := []string{"a", "c", "d", "b", "e"}
	if len(got) != len(want) {
		t.Fatalf("latest = %+v; want texts %v", got, want)
	}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("latest[%d] = %+v; want text %q", i, got[i], w)
		}
	}
	if got := latestDecisions(nil, 5); got == nil || len(got) != 0 {
		t.Errorf("latestDecisions(nil) = %v; want empty non-nil slice", got)
	}
}

// fixtureWorkspace builds a temp workspace exercising every discovery rule:
// a root repo (never an initiative), a hidden dir, an unmarked group, and
// two initiatives — one rich (members, notes, .beads), one bare.
func fixtureWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{ws}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Root repo with a stray INITIATIVE.md — .git wins, never an initiative.
	repo := mk("zdev")
	mk("zdev", ".git")
	write(filepath.Join(repo, "INITIATIVE.md"), "**Intent:** trap\n")

	mk(".hidden")
	mk("projects") // unmarked group — invisible to the digest

	// Rich initiative.
	rich := mk("marketplace")
	write(filepath.Join(rich, "INITIATIVE.md"),
		"# marketplace\n\n**Intent:** Ship the MVP —\nthe wrapped tail.\n**Started:** 2026-07-30\n**Tracker:** TBD\n\n"+
			"## Decisions\n\n- 2026-07-30 — first\n- 2026-08-01 — second\n  with continuation\n- 2026-08-02 — third\n"+
			"- 2026-08-03 — fourth\n- 2026-08-03 — fifth\n- 2026-08-04 — sixth\n\n"+
			"## Outcome\n<!-- filled at wind-down -->\n")
	mk("marketplace", "pay-app", ".git")
	mk("marketplace", "pay-ops", ".git")
	mk("marketplace", "notes")  // never a member: no .git
	mk("marketplace", ".beads") // triggers the bd seam
	write(filepath.Join(rich, "notes", "BREAKDOWN.md"), "x")
	write(filepath.Join(rich, "notes", "IDEAS.md"), "x")
	mk("marketplace", "notes", "sub") // dirs in notes/ are not note files

	// Bare initiative: marker only.
	bare := mk("empty-init")
	write(filepath.Join(bare, "INITIATIVE.md"), "# empty-init\n")

	return ws
}

func TestCollectDigest(t *testing.T) {
	ws := fixtureWorkspace(t)
	unpushed := int64(2)
	r := &initiativesRunner{
		workspace: ws,
		runGit: func(dir string, args ...string) (string, error) {
			switch args[0] {
			case "rev-parse":
				return "marketplace/" + filepath.Base(dir) + "\n", nil
			case "status":
				if filepath.Base(dir) == "pay-app" {
					return " M file.go\n", nil
				}
				return "", nil
			case "rev-list":
				if filepath.Base(dir) == "pay-app" {
					return fmt.Sprintf("%d\n", unpushed), nil
				}
				return "", fmt.Errorf("no upstream")
			case "log":
				return "1700000000\n", nil
			}
			return "", fmt.Errorf("unexpected git %v", args)
		},
		runBd: func(dir, beadsDir string) ([]byte, error) {
			if filepath.Base(beadsDir) != ".beads" || filepath.Dir(beadsDir) != dir {
				t.Errorf("bd seam: dir=%q beadsDir=%q", dir, beadsDir)
			}
			return []byte(`{"schema_version":1,"summary":{"open_issues":88,"ready_issues":52,"in_progress_issues":1,"blocked_issues":36}}`), nil
		},
	}

	d, err := r.collect(1234567890)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || d.GeneratedAt != 1234567890 || d.Workspace != ws {
		t.Errorf("header = %+v", d)
	}
	if len(d.Initiatives) != 2 {
		t.Fatalf("initiatives = %+v; want empty-init + marketplace", d.Initiatives)
	}

	// ReadDir order: empty-init sorts before marketplace.
	bare := d.Initiatives[0]
	if bare.Name != "empty-init" || bare.Intent != nil || bare.Work != nil ||
		len(bare.Members) != 0 || len(bare.Notes) != 0 || bare.Decisions.Count != 0 {
		t.Errorf("bare initiative = %+v", bare)
	}

	rich := d.Initiatives[1]
	if rich.Name != "marketplace" || rich.Path != filepath.Join(ws, "marketplace") {
		t.Errorf("identity = %q %q", rich.Name, rich.Path)
	}
	assertStrPtr(t, "intent", rich.Intent, strPtr("Ship the MVP — the wrapped tail."))
	assertStrPtr(t, "started", rich.Started, strPtr("2026-07-30"))
	assertStrPtr(t, "tracker", rich.Tracker, strPtr("TBD"))
	assertStrPtr(t, "outcome", rich.Outcome, nil)

	if rich.Decisions.Count != 6 || len(rich.Decisions.Latest) != 5 {
		t.Fatalf("decisions = %+v", rich.Decisions)
	}
	if rich.Decisions.Latest[0].Text != "second with continuation" ||
		rich.Decisions.Latest[4].Date != "2026-08-04" {
		t.Errorf("latest = %+v; want oldest-first slice of the 5 most recent", rich.Decisions.Latest)
	}

	if len(rich.Members) != 2 {
		t.Fatalf("members = %+v; want pay-app + pay-ops (notes/ excluded)", rich.Members)
	}
	app := rich.Members[0]
	if app.Name != "pay-app" || app.Branch != "marketplace/pay-app" || !app.Dirty ||
		app.Unpushed == nil || *app.Unpushed != 2 || app.LastCommitAt == nil || *app.LastCommitAt != 1700000000 {
		t.Errorf("pay-app = %+v", app)
	}
	ops := rich.Members[1]
	if ops.Dirty || ops.Unpushed != nil {
		t.Errorf("pay-ops = %+v; want clean with null unpushed (no upstream)", ops)
	}

	if rich.Work == nil || *rich.Work != (workDigest{Tool: "bd", Open: 88, Ready: 52, InProgress: 1, Blocked: 36}) {
		t.Errorf("work = %+v", rich.Work)
	}
	if len(rich.Notes) != 2 || rich.Notes[0] != "BREAKDOWN.md" || rich.Notes[1] != "IDEAS.md" {
		t.Errorf("notes = %v", rich.Notes)
	}
}

// TestCollectDigest_JSONContract locks the v1 wire shape: field NAMES and
// null semantics, not just Go-side values — downstream /plan is written
// against these exact keys.
func TestCollectDigest_JSONContract(t *testing.T) {
	ws := fixtureWorkspace(t)
	r := &initiativesRunner{
		workspace: ws,
		runGit:    func(dir string, args ...string) (string, error) { return "", fmt.Errorf("fake .git") },
		runBd:     func(dir, beadsDir string) ([]byte, error) { return nil, fmt.Errorf("bd missing") },
	}
	d, err := r.collect(42)
	if err != nil {
		t.Fatal(err)
	}
	out, err := formatInitiativesJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for _, k := range []string{"version", "generatedAt", "workspace", "initiatives"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("top-level key %q missing", k)
		}
	}
	inits := raw["initiatives"].([]any)
	entry := inits[1].(map[string]any) // marketplace
	for _, k := range []string{"name", "path", "intent", "started", "tracker", "outcome",
		"decisions", "members", "work", "notes"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("initiative key %q missing", k)
		}
	}
	// bd errored ⇒ work is null, not absent and not an error.
	if entry["work"] != nil {
		t.Errorf("work = %v; want null when bd fails", entry["work"])
	}
	if entry["outcome"] != nil {
		t.Errorf("outcome = %v; want null for comment-only section", entry["outcome"])
	}
	member := entry["members"].([]any)[0].(map[string]any)
	for _, k := range []string{"name", "branch", "dirty", "unpushed", "lastCommitAt"} {
		if _, ok := member[k]; !ok {
			t.Errorf("member key %q missing", k)
		}
	}
	// Fake .git dirs: every git call failed ⇒ branch "", unpushed null,
	// lastCommitAt null — degraded, never an error.
	if member["branch"] != "" || member["unpushed"] != nil || member["lastCommitAt"] != nil {
		t.Errorf("degraded member = %v", member)
	}
	// Bare initiative: members/notes/latest are [], never null.
	bare := inits[0].(map[string]any)
	if bare["members"] == nil || bare["notes"] == nil {
		t.Errorf("bare arrays must be [] not null: %v", bare)
	}
	if bare["decisions"].(map[string]any)["latest"] == nil {
		t.Errorf("decisions.latest must be [] not null: %v", bare["decisions"])
	}
}

func TestCollectDigest_EmptyWorkspace(t *testing.T) {
	r := &initiativesRunner{workspace: t.TempDir()}
	d, err := r.collect(1)
	if err != nil {
		t.Fatal(err)
	}
	out, err := formatInitiativesJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"initiatives":[]`) {
		t.Errorf("empty workspace must serialize initiatives as []: %s", out)
	}
}

// TestDeriveMember_LiveGit exercises the real git seam against a throwaway
// repo: branch name, dirty flag, unborn HEAD (null lastCommitAt), and the
// no-upstream null. Everything local — no remote is ever configured, so a
// network touch would fail loudly here.
func TestDeriveMember_LiveGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "market/x")
	r := &initiativesRunner{runGit: defaultRunGit}

	// Unborn HEAD: branch known via symbolic-ref, no commit timestamp.
	m := r.deriveMember(dir)
	if m.Branch != "market/x" || m.LastCommitAt != nil {
		t.Errorf("unborn = %+v; want branch market/x, null lastCommitAt", m)
	}

	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "c1")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}

	m = r.deriveMember(dir)
	if m.Branch != "market/x" || !m.Dirty {
		t.Errorf("committed = %+v; want dirty on market/x", m)
	}
	if m.Unpushed != nil {
		t.Errorf("unpushed = %v; want null with no upstream", *m.Unpushed)
	}
	if m.LastCommitAt == nil || *m.LastCommitAt != 1767225600 { // 2026-01-01T00:00:00Z
		t.Errorf("lastCommitAt = %v; want pinned committer date", m.LastCommitAt)
	}
}

func TestFormatInitiatives(t *testing.T) {
	two := int64(2)
	d := &initiativesDigest{
		Workspace: "/w",
		Initiatives: []initiativeDigest{{
			Name:    "marketplace",
			Intent:  strPtr("Ship the MVP"),
			Started: strPtr("2026-07-30"),
			Members: []memberDigest{
				{Name: "a", Dirty: true, Unpushed: &two},
				{Name: "b"},
			},
			Work: &workDigest{Tool: "bd", Open: 88, Ready: 52, Blocked: 36},
			Decisions: decisionsDigest{Count: 6, Latest: []decision{
				{Date: "2026-08-03", Text: "x"},
			}},
		}},
	}
	// 2026-08-04 00:00Z, 5 days after Started.
	got := formatInitiatives(d, 1785801600)
	for _, want := range []string{
		"marketplace", "Ship the MVP",
		"started 2026-07-30", "2 members (1 dirty, 2 unpushed)",
		"bd: 88 open / 52 ready / 36 blocked",
		"6 decisions, last 2026-08-03",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatInitiatives missing %q\ngot:\n%s", want, got)
		}
	}

	empty := &initiativesDigest{Workspace: "/w", Initiatives: []initiativeDigest{}}
	if got := formatInitiatives(empty, 0); got != "(no initiatives under /w)\n" {
		t.Errorf("empty = %q", got)
	}
}
