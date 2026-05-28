package tmuxctl

// OutputSink receives the decoded byte payload of a `%output` line keyed by
// pane id. Phase 2 default is discardSink{} — the sink interface exists to
// avoid a Phase 3 retrofit if a use case emerges (D2-11). The parser holds
// a reference and calls Output(paneID, decoded) for every successfully-
// parsed %output line.
type OutputSink interface {
	Output(paneID string, payload []byte)
}

// discardSink is the no-op default. Phase 2 is content with this; agent-
// state detection in Phase 2 flows through pane TITLES (via %subscription-
// changed), not pane scrollback content (via %output).
type discardSink struct{}

func (discardSink) Output(_ string, _ []byte) {}
