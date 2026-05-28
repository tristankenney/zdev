package socket

import (
	"bufio"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// TestDialDiagRoundtrip verifies the client side of D4-07 against a hand-rolled
// stub server: accept one conn, read one line, write one canned diag-reply,
// close. DialDiag must parse the reply and return populated fields.
func TestDialDiagRoundtrip(t *testing.T) {
	dir := shortTempSocketDir(t)
	path := filepath.Join(dir, "stub.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	const cannedReply = `{"type":"diag-reply","v":1,"uptime_sec":1.5,"started_at":"2026-05-05T00:00:00Z","last_event_ago_sec":0.1,"schema":"phase4-v1","subscribers":2,"queue_depth":0,"errors_1h":0,"socket":"/tmp/x"}`

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		// Read one line (the diag request).
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 4096), 64*1024)
		_ = sc.Scan()
		// Write the canned reply.
		_, _ = conn.Write([]byte(cannedReply + "\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := DialDiag(ctx, path)
	if err != nil {
		t.Fatalf("DialDiag: %v", err)
	}
	<-srvDone

	if r.Type != "diag-reply" {
		t.Errorf("Type = %q, want diag-reply", r.Type)
	}
	if r.V != 1 {
		t.Errorf("V = %d, want 1", r.V)
	}
	if r.UptimeSec != 1.5 {
		t.Errorf("UptimeSec = %v, want 1.5", r.UptimeSec)
	}
	if r.Schema != "phase4-v1" {
		t.Errorf("Schema = %q, want phase4-v1", r.Schema)
	}
	if r.Subscribers != 2 {
		t.Errorf("Subscribers = %d, want 2", r.Subscribers)
	}
	if r.Socket != "/tmp/x" {
		t.Errorf("Socket = %q, want /tmp/x", r.Socket)
	}
}

// TestDialDiagTimeout verifies that DialDiag respects snapshotReadTimeout
// (1s). If the server accepts but writes nothing, DialDiag must fail within
// snapshotReadTimeout + a small slop window — never block indefinitely.
func TestDialDiagTimeout(t *testing.T) {
	dir := shortTempSocketDir(t)
	path := filepath.Join(dir, "silent.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		// Hold the conn open without writing; close after the test deadline
		// so the test goroutine doesn't leak.
		time.Sleep(snapshotReadTimeout + 500*time.Millisecond)
		_ = conn.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err = DialDiag(ctx, path)
	elapsed := time.Since(start)
	<-srvDone

	if err == nil {
		t.Fatal("DialDiag returned nil err on silent server; want timeout error")
	}
	// Allow generous slop — race detector adds latency.
	if elapsed > snapshotReadTimeout+1*time.Second {
		t.Errorf("DialDiag returned after %v; want ≤ %v", elapsed, snapshotReadTimeout+1*time.Second)
	}
}
