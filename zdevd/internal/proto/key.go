package proto

import "strings"

// SessionKey converts a slash-form project name (e.g. "example/.github") to
// the dash-form key used to look up data keyed by tmux session name.
//
// Two substitutions are required:
//   - "/" → "-": tmux session names use dashes for the project's path
//     separator (D-02 / Phase 999.1 convention).
//   - "." → "_": tmux normalizes "." → "_" when creating sessions and treats
//     "." as a target-spec separator in `has-session -t`, so any "." in a
//     project name would silently mismatch between zdev and the daemon.
func SessionKey(name string) string {
	s := strings.ReplaceAll(name, "/", "-")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}
