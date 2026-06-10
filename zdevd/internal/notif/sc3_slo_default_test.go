//go:build !live

package notif_test

// sc3SLOStrict gates the SC3 latency-budget ASSERTIONS (the functional
// "event arrived" checks are unconditional). In normal `go test` runs —
// the pre-push hook and CI, where the suite runs in parallel under
// race-detector load — wall-clock latency is dominated by the scheduler,
// and the 100ms SLO flaked the gate repeatedly (issues #2/#3 lineage:
// three blocked pushes on 2026-06-10 alone). Breaches are logged, not
// failed; `make -C zdevd live-test` (-tags live) enforces the SLO on a
// machine expected to be idle.
const sc3SLOStrict = false
