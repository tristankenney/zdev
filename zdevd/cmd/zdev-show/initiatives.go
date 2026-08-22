package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// `zdev-show initiatives` — the machine-readable initiative digest.
//
// This subcommand NEVER dials the daemon: everything it reports is derived
// from the workspace layout (the same on-disk registry bin/zdev's discovery
// walks), INITIATIVE.md files, LOCAL git state, and an optional `bd stats`
// subprocess. It exists so downstream consumers (the /plan skill, scripts,
// widgets) read initiative state through ONE stable contract instead of
// each growing its own INITIATIVE.md parser or bd invocation.
//
// The --json output is a versioned consumer contract (initiativesDigest,
// version 1) documented in docs/initiatives-digest.md. Field names and
// semantics are frozen; breaking changes bump Version. Everything here is
// local-only — no fetch, no network, ever.

// initiativesDigestVersion is the frozen contract version. Bump ONLY on a
// breaking change to documented field names or semantics; additive fields
// do not bump it (consumers must ignore unknown fields).
const initiativesDigestVersion = 1

// initiativeGitTimeout bounds each git/bd subprocess. All of them are
// local reads that finish in milliseconds — the timeout exists to bound a
// pathologically hung subprocess (NFS mount, wedged index lock), matching
// the probes package's "local, should be quick" budget.
const initiativeGitTimeout = 10 * time.Second

// initiativesDigest is the top-level --json wire shape (contract v1).
type initiativesDigest struct {
	Version     int                `json:"version"`
	GeneratedAt int64              `json:"generatedAt"`
	Workspace   string             `json:"workspace"`
	Initiatives []initiativeDigest `json:"initiatives"`
}

// initiativeDigest is one initiative's entry. Pointer fields are `null` on
// the wire when the underlying INITIATIVE.md piece is absent — a missing
// section is data ("not filled in yet"), never an error.
type initiativeDigest struct {
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Intent    *string         `json:"intent"`
	Started   *string         `json:"started"`
	Tracker   *string         `json:"tracker"`
	Outcome   *string         `json:"outcome"`
	Decisions decisionsDigest `json:"decisions"`
	Members   []memberDigest  `json:"members"`
	Work      *workDigest     `json:"work"`
	Notes     []string        `json:"notes"`
}

// decisionsDigest carries the total count plus up to the 5 most recent
// decisions by date (ties keep file order), oldest-first within Latest —
// so a consumer prints Latest verbatim and gets chronological order.
type decisionsDigest struct {
	Count  int        `json:"count"`
	Latest []decision `json:"latest"`
}

type decision struct {
	Date string `json:"date"`
	Text string `json:"text"`
}

// memberDigest is one member clone's LOCAL git state. Unpushed is null when
// the branch has no upstream (unpushed-ness is unknowable, not zero);
// LastCommitAt is null on an unborn HEAD. Branch is "" when underivable
// (corrupt or fake .git) and "detached" on a detached HEAD.
type memberDigest struct {
	Name         string `json:"name"`
	Branch       string `json:"branch"`
	Dirty        bool   `json:"dirty"`
	Unpushed     *int64 `json:"unpushed"`
	LastCommitAt *int64 `json:"lastCommitAt"`
	// Stream names the WORKSTREAM folder this member lives in (decision
	// 2026-08-17 rev 2: a workstream is an unmarked child folder of the
	// initiative holding full clones — one pay-cli stack, one runner,
	// one DNS namespace <service>.<init>-<stream>.localhost). Member
	// Name is then "<stream>/<repo>"; empty for direct members.
	// Additive to the v1 contract — see docs/initiatives-digest.md.
	Stream string `json:"stream,omitempty"`
}

// workDigest maps `bd stats --json` summary counts. Tool is always "bd"
// in v1; the field exists so a future tracker can slot in without a shape
// change.
type workDigest struct {
	Tool       string `json:"tool"`
	Open       int    `json:"open"`
	Ready      int    `json:"ready"`
	InProgress int    `json:"inProgress"`
	Blocked    int    `json:"blocked"`
}

// ---------------------------------------------------------------------------
// INITIATIVE.md parsing (pure — no filesystem, no clock)

// initiativeDoc is the parsed form of one INITIATIVE.md. Nil pointer =
// the piece is absent from the file.
type initiativeDoc struct {
	Intent    *string
	Started   *string
	Tracker   *string
	Outcome   *string
	Decisions []decision
}

// boldFieldRe matches the template's "**Field:** value" metadata lines
// (Intent / Started / Tracker). Anchored to line start; the value may be
// empty on the field line itself and wrap onto following lines.
var boldFieldRe = regexp.MustCompile(`^\*\*([A-Za-z][A-Za-z0-9 _-]*):\*\*\s*(.*)$`)

// decisionRe matches a dated decision bullet: "- YYYY-MM-DD — text" with
// either an em-dash or a plain hyphen after the date.
var decisionRe = regexp.MustCompile(`^-\s+(\d{4}-\d{2}-\d{2})\s*(?:—|-)\s*(.*)$`)

// htmlCommentRe strips HTML comments (the template's placeholder text)
// from section bodies. (?s) so a comment may span lines.
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// sectionRe matches a level-2 heading ("## Decisions", "## Outcome").
var sectionRe = regexp.MustCompile(`^##\s+(.+?)\s*$`)

// parseInitiativeMD tolerantly extracts the digest-relevant pieces from an
// INITIATIVE.md body. Missing pieces come back nil/empty — this function
// never fails, because a half-filled INITIATIVE.md is a normal state for a
// young initiative, not an error the whole digest should refuse over.
func parseInitiativeMD(b []byte) initiativeDoc {
	var doc initiativeDoc
	lines := strings.Split(string(b), "\n")

	// Pass 1: bold metadata fields, with wrapping. A field's value starts
	// on its "**Field:**" line and continues over following lines until a
	// blank line, the next bold field, or a heading.
	fields := map[string]*string{}
	var curField string
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if m := boldFieldRe.FindStringSubmatch(line); m != nil {
			curField = strings.ToLower(strings.TrimSpace(m[1]))
			v := strings.TrimSpace(m[2])
			fields[curField] = &v
			continue
		}
		if curField == "" {
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			curField = ""
			continue
		}
		joined := strings.TrimSpace(*fields[curField] + " " + strings.TrimSpace(line))
		fields[curField] = &joined
	}
	doc.Intent = fields["intent"]
	doc.Started = fields["started"]
	doc.Tracker = fields["tracker"]

	// Pass 2: sections. Decisions are dated bullets (continuation lines —
	// anything indented and non-blank, including nested sub-bullets — join
	// the current decision with spaces). Outcome is the section body with
	// the template's HTML comment stripped; whitespace-only ⇒ nil.
	section := ""
	var outcomeLines []string
	var cur *decision
	flush := func() {
		if cur != nil {
			doc.Decisions = append(doc.Decisions, *cur)
			cur = nil
		}
	}
	for _, raw := range lines {
		if m := sectionRe.FindStringSubmatch(raw); m != nil {
			flush()
			section = strings.ToLower(strings.TrimSpace(m[1]))
			continue
		}
		if strings.HasPrefix(raw, "#") && !strings.HasPrefix(raw, "##") {
			flush()
			section = ""
			continue
		}
		switch section {
		case "decisions":
			line := strings.TrimRight(raw, " \t")
			switch {
			case decisionRe.MatchString(line):
				flush()
				m := decisionRe.FindStringSubmatch(line)
				cur = &decision{Date: m[1], Text: strings.TrimSpace(m[2])}
			case cur != nil && strings.TrimSpace(line) != "" &&
				(strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t")):
				cur.Text = strings.TrimSpace(cur.Text + " " + strings.TrimSpace(line))
			default:
				// Blank line, undated bullet, or unindented prose: the
				// current bullet is complete. Undated bullets are not
				// decisions and never attach to the previous one.
				flush()
			}
		case "outcome":
			outcomeLines = append(outcomeLines, raw)
		}
	}
	flush()

	if len(outcomeLines) > 0 {
		body := htmlCommentRe.ReplaceAllString(strings.Join(outcomeLines, "\n"), "")
		body = strings.TrimSpace(body)
		if body != "" {
			doc.Outcome = &body
		}
	}
	return doc
}

// latestDecisions returns up to max most-recent decisions by date (ties
// keep file order), oldest-first within the returned slice. Never nil —
// `[]` on the wire when there are no decisions.
func latestDecisions(all []decision, max int) []decision {
	sorted := make([]decision, len(all))
	copy(sorted, all)
	sort.SliceStable(sorted, func(i, j int) bool {
		// ISO dates compare correctly as strings.
		return sorted[i].Date < sorted[j].Date
	})
	if len(sorted) > max {
		sorted = sorted[len(sorted)-max:]
	}
	return sorted
}

// ---------------------------------------------------------------------------
// Digest assembly (filesystem walk + subprocess derivation)

// initiativesRunner assembles the digest. The exec seams (runGit / runBd)
// exist so tests can fixture a workspace with fake .git dirs and stubbed
// subprocess results — the same seam style internal/probes uses.
type initiativesRunner struct {
	workspace string
	runGit    func(dir string, args ...string) (string, error)
	runBd     func(dir, beadsDir string) ([]byte, error)
}

// newInitiativesRunner resolves the workspace exactly like bin/zdev and
// cmd/zdevd do: ZDEV_WORKSPACE (gap-filled from ~/.config/zdev/env by the
// caller's ApplyUserEnv) or ~/workspace.
func newInitiativesRunner() *initiativesRunner {
	ws := os.Getenv("ZDEV_WORKSPACE")
	if ws == "" {
		ws = filepath.Join(os.Getenv("HOME"), "workspace")
	}
	return &initiativesRunner{
		workspace: ws,
		runGit:    defaultRunGit,
		runBd:     defaultRunBd,
	}
}

// collect walks the workspace root and assembles the digest. The only hard
// error is an unreadable workspace root — everything below that degrades
// per-piece (nil fields, work:null) rather than failing the whole digest.
// now is threaded in per the repo convention: derivation logic never calls
// time.Now itself.
func (r *initiativesRunner) collect(now int64) (*initiativesDigest, error) {
	entries, err := os.ReadDir(r.workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", r.workspace, err)
	}
	digest := &initiativesDigest{
		Version:     initiativesDigestVersion,
		GeneratedAt: now,
		Workspace:   r.workspace,
		Initiatives: []initiativeDigest{},
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(r.workspace, e.Name())
		// Mirror bin/zdev discovery: a root dir WITH .git is a project,
		// never an initiative — even if someone drops an INITIATIVE.md
		// into a repo.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, "INITIATIVE.md")); err != nil || fi.IsDir() {
			continue
		}
		digest.Initiatives = append(digest.Initiatives, r.collectOne(e.Name(), dir))
	}
	return digest, nil
}

// collectOne assembles a single initiative's entry.
func (r *initiativesRunner) collectOne(name, dir string) initiativeDigest {
	init := initiativeDigest{
		Name:    name,
		Path:    dir,
		Members: []memberDigest{},
		Notes:   []string{},
	}

	if b, err := os.ReadFile(filepath.Join(dir, "INITIATIVE.md")); err == nil {
		doc := parseInitiativeMD(b)
		init.Intent = doc.Intent
		init.Started = doc.Started
		init.Tracker = doc.Tracker
		init.Outcome = doc.Outcome
		init.Decisions = decisionsDigest{
			Count:  len(doc.Decisions),
			Latest: latestDecisions(doc.Decisions, 5),
		}
	} else {
		init.Decisions = decisionsDigest{Latest: []decision{}}
	}

	// Members: non-hidden child dirs containing .git (file or dir — clones
	// or worktrees), the same rule discovery uses for initiative-member rows.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			mDir := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(mDir, ".git")); err == nil {
				init.Members = append(init.Members, r.deriveMember(mDir))
				continue
			}
			if e.Name() == "notes" {
				continue
			}
			// Unmarked child without .git: a WORKSTREAM folder — its
			// repo children are members named <stream>/<repo> with the
			// Stream field set (see memberDigest.Stream).
			subs, err := os.ReadDir(mDir)
			if err != nil {
				continue
			}
			for _, sub := range subs {
				if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") {
					continue
				}
				sDir := filepath.Join(mDir, sub.Name())
				if _, err := os.Stat(filepath.Join(sDir, ".git")); err != nil {
					continue
				}
				m := r.deriveMember(sDir)
				m.Name = e.Name() + "/" + sub.Name()
				m.Stream = e.Name()
				init.Members = append(init.Members, m)
			}
		}
	}

	// Work: optional bd. Missing binary, missing .beads, or any bd failure
	// ⇒ work is null — the digest must not fail because a tracker is absent.
	if fi, err := os.Stat(filepath.Join(dir, ".beads")); err == nil && fi.IsDir() {
		if out, err := r.runBd(dir, filepath.Join(dir, ".beads")); err == nil {
			init.Work = parseBdStats(out)
		}
	}

	// Notes: filenames directly in notes/, sorted (ReadDir sorts).
	if entries, err := os.ReadDir(filepath.Join(dir, "notes")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			init.Notes = append(init.Notes, e.Name())
		}
	}
	return init
}

// deriveMember reads one member clone's LOCAL git state. Every command is
// a local read (rev-parse, status, rev-list against the already-fetched
// upstream ref, log) — this function must never fetch or otherwise touch
// the network. Each piece degrades independently on error.
func (r *initiativesRunner) deriveMember(dir string) memberDigest {
	m := memberDigest{Name: filepath.Base(dir)}

	if out, err := r.runGit(dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		br := strings.TrimSpace(out)
		if br == "HEAD" {
			br = "detached"
		}
		m.Branch = br
	} else if out, err := r.runGit(dir, "symbolic-ref", "--short", "HEAD"); err == nil {
		// Unborn branch: rev-parse has no commit to resolve, but the
		// symbolic ref still names the branch.
		m.Branch = strings.TrimSpace(out)
	}

	if out, err := r.runGit(dir, "status", "--porcelain"); err == nil && strings.TrimSpace(out) != "" {
		m.Dirty = true
	}

	// @{upstream} resolution fails when the branch has no upstream — that
	// is the null case (unknowable), distinct from a real 0 (fully pushed).
	if out, err := r.runGit(dir, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
		if n, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); perr == nil {
			m.Unpushed = &n
		}
	}

	if out, err := r.runGit(dir, "log", "-1", "--format=%ct"); err == nil {
		if ts, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64); perr == nil {
			m.LastCommitAt = &ts
		}
	}
	return m
}

// parseBdStats maps `bd stats --json` summary counts into the contract
// shape. Unparsable output ⇒ nil (work: null) — bd's exact output contract
// is not something this digest should be brittle against.
func parseBdStats(out []byte) *workDigest {
	var payload struct {
		Summary struct {
			Open       int `json:"open_issues"`
			Ready      int `json:"ready_issues"`
			InProgress int `json:"in_progress_issues"`
			Blocked    int `json:"blocked_issues"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil
	}
	return &workDigest{
		Tool:       "bd",
		Open:       payload.Summary.Open,
		Ready:      payload.Summary.Ready,
		InProgress: payload.Summary.InProgress,
		Blocked:    payload.Summary.Blocked,
	}
}

func defaultRunGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), initiativeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func defaultRunBd(dir, beadsDir string) ([]byte, error) {
	if _, err := exec.LookPath("bd"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), initiativeGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", "stats", "--json")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	return cmd.Output()
}

// ---------------------------------------------------------------------------
// Output

// formatInitiativesJSON marshals the digest one-line, matching the other
// --json surfaces (review/triage/held).
func formatInitiativesJSON(d *initiativesDigest) (string, error) {
	b, err := json.Marshal(d)
	return string(b), err
}

// formatInitiatives renders the compact human view: a dim header per
// initiative (matching formatShow's frame), the intent on one line, then
// one dim metadata line. Missing pieces are simply omitted — the human
// view answers "what's alive and where is it up to" at a glance; the JSON
// contract carries the full detail.
func formatInitiatives(d *initiativesDigest, now int64) string {
	if len(d.Initiatives) == 0 {
		return fmt.Sprintf("(no initiatives under %s)\n", d.Workspace)
	}
	var b strings.Builder
	for _, init := range d.Initiatives {
		fmt.Fprintf(&b, "%s─── %s ──%s\n", dim, init.Name, reset)
		if init.Intent != nil {
			fmt.Fprintf(&b, "  %s\n", truncateRunes(*init.Intent, 76))
		}
		fmt.Fprintf(&b, "  %s%s%s\n", dim, initiativeMetaLine(init, now), reset)
	}
	return b.String()
}

// initiativeMetaLine composes the dim per-initiative summary: age, member
// count with dirty/unpushed rollup, work counts, latest decision date, and
// a wind-down marker when Outcome is filled.
func initiativeMetaLine(init initiativeDigest, now int64) string {
	var parts []string

	if init.Started != nil {
		age := ""
		if t, err := time.Parse("2006-01-02", strings.TrimSpace(*init.Started)); err == nil {
			age = fmt.Sprintf(" (%s)", formatAge(now-t.Unix()))
		}
		parts = append(parts, fmt.Sprintf("started %s%s", *init.Started, age))
	}

	streamSet := map[string]bool{}
	var streamed int
	for _, m := range init.Members {
		if m.Stream != "" {
			streamSet[m.Stream] = true
			streamed++
		}
	}
	base := len(init.Members) - streamed
	member := fmt.Sprintf("%d members", base)
	if base == 1 {
		member = "1 member"
	}
	if n := len(streamSet); n > 0 {
		if n == 1 {
			member += " · 1 stream"
		} else {
			member += fmt.Sprintf(" · %d streams", n)
		}
	}
	var dirty int
	var unpushed int64
	for _, m := range init.Members {
		if m.Dirty {
			dirty++
		}
		if m.Unpushed != nil {
			unpushed += *m.Unpushed
		}
	}
	var flags []string
	if dirty > 0 {
		flags = append(flags, fmt.Sprintf("%d dirty", dirty))
	}
	if unpushed > 0 {
		flags = append(flags, fmt.Sprintf("%d unpushed", unpushed))
	}
	if len(flags) > 0 {
		member += " (" + strings.Join(flags, ", ") + ")"
	}
	parts = append(parts, member)

	if w := init.Work; w != nil {
		var counts []string
		if w.Open > 0 {
			counts = append(counts, fmt.Sprintf("%d open", w.Open))
		}
		if w.Ready > 0 {
			counts = append(counts, fmt.Sprintf("%d ready", w.Ready))
		}
		if w.InProgress > 0 {
			counts = append(counts, fmt.Sprintf("%d wip", w.InProgress))
		}
		if w.Blocked > 0 {
			counts = append(counts, fmt.Sprintf("%d blocked", w.Blocked))
		}
		if len(counts) == 0 {
			counts = append(counts, "no open work")
		}
		parts = append(parts, w.Tool+": "+strings.Join(counts, " / "))
	}

	if n := len(init.Decisions.Latest); n > 0 {
		parts = append(parts, fmt.Sprintf("%d decisions, last %s",
			init.Decisions.Count, init.Decisions.Latest[n-1].Date))
	}

	if init.Outcome != nil {
		parts = append(parts, "outcome recorded")
	}
	return strings.Join(parts, " · ")
}

// truncateRunes shortens s to at most max runes, appending an ellipsis —
// rune-safe because intents routinely contain em-dashes.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
