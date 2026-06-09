package tmuxctl

// Compile-time interface satisfaction checks for Phase 3 events.
var (
	_ Event = DataRefresh{}
	_ Event = PRRefresh{}
	_ Event = PortsRefresh{}
	_ Event = NotifSeen{}
	_ Event = ProjectListChanged{}
	_ Event = PaneCommandChanged{}
	_ Event = ActivityRefresh{}
	_ Event = PaneCwdChanged{}
)
