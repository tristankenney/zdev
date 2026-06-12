package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestRenderTriageSection_MemberLabelSkipped pins the consumer half of the
// Agent Teams slice C triage contract: rankTriage now emits "lead/member"
// labels for waiting team members, and the strip must SKIP any queue entry
// with no matching Projects[].Name — no panic, no blank row, output
// byte-identical to the same queue without the member entry.
func TestRenderTriageSection_MemberLabelSkipped(t *testing.T) {
	old := TriageStripEnabled
	TriageStripEnabled = true
	defer func() { TriageStripEnabled = old }()

	base := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: 100},
		},
	}
	now := func() int64 { return int64(200) }
	an := NewAnimator()

	var withMember, without bytes.Buffer
	base.Triage = []string{"alpha", "alpha/blk"}
	rowsWith := renderTriageSection(&withMember, base, 80, an, now)
	base.Triage = []string{"alpha"}
	rowsWithout := renderTriageSection(&without, base, 80, an, now)

	if rowsWith != rowsWithout {
		t.Errorf("member label changed row count: with=%d without=%d", rowsWith, rowsWithout)
	}
	if withMember.String() != without.String() {
		t.Errorf("member label changed strip bytes:\nwith:    %q\nwithout: %q",
			withMember.String(), without.String())
	}
	if strings.Contains(withMember.String(), "blk") {
		t.Errorf("member label leaked into the strip: %q", withMember.String())
	}
}
