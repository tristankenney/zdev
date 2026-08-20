// M1 trust boundary: applyEvent scrubs terminal control bytes out of the
// untrusted text it ingests (agent hook summaries, MCP-set titles) so no
// ESC/CSI/OSC/CR/BEL can reach the operator's sidebar, the persisted state
// file, or the phone push. These tests lock the sanitization to the ingestion
// sites; the scrubber's own contract is covered in internal/render.
package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

const osc52 = "\x1b]52;c;aGVsbG8=\x07" // OSC 52 clipboard-write escape

func assertClean(t *testing.T, field, got string) {
	t.Helper()
	if strings.ContainsAny(got, "\x1b\x07\r\n") {
		t.Errorf("%s still carries control bytes: %q", field, got)
	}
}

func TestApplyEvent_NotifSeen_SanitizesWaitSummary(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.NotifSeen{
		Session:   "proj",
		Kind:      proto.WaitKindPermission,
		Summary:   "Allow Bash?" + osc52 + "\rFAKE",
		Timestamp: 1_800_000_000,
	}, nil)

	got := s.projectData["proj"].WaitSummary
	assertClean(t, "WaitSummary", got)
	if got != "Allow Bash?]52;c;aGVsbG8=FAKE" {
		t.Errorf("WaitSummary = %q", got)
	}
}

func TestApplyEvent_NotifSeen_SanitizesDeadReason(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.NotifSeen{
		Session:   "proj",
		Kind:      proto.WaitKindDead,
		Summary:   "exit 1" + osc52,
		Timestamp: 1_800_000_000,
	}, nil)

	got := s.projectData["proj"].DeadReason
	assertClean(t, "DeadReason", got)
	if got != "exit 1]52;c;aGVsbG8=" {
		t.Errorf("DeadReason = %q", got)
	}
}

func TestApplyEvent_ParkText_SanitizesTitle(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.ParkText{Text: "call\x1b[31m dentist" + osc52, NowNanos: int64(time.Second)}, nil)

	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want 1", s.heldItems)
	}
	assertClean(t, "HeldItem.Title", s.heldItems[0].Title)
}

func TestApplyEvent_AnchorSet_SanitizesTitleAndProject(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{
		Title:    "ship it" + osc52,
		Project:  "proj\r\nFAKE",
		NowNanos: int64(time.Second),
	}, nil)

	if s.anchor == nil {
		t.Fatal("anchor not set")
	}
	assertClean(t, "Anchor.Title", s.anchor.Title)
	assertClean(t, "Anchor.Project", s.anchor.Project)
}
