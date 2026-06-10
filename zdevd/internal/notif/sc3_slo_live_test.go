//go:build live

package notif_test

// Under -tags live the SC3 latency SLO is a hard assertion — see the
// !live twin for why normal runs only log breaches.
const sc3SLOStrict = true
