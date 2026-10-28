package grpc

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	genv1 "github.com/vortexui/vortexui/internal/transport/genv1"
)

// TestNodeProbeDialsFromTheAgent proves the probe reports what the node can
// actually reach: an open port succeeds with latency, a closed one fails with a
// reason rather than an RPC error.
func TestNodeProbeDialsFromTheAgent(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	srv := NewNodeServer(nil, "test", nil)
	ctx := context.Background()

	open, err := srv.Probe(ctx, &genv1.ProbeRequest{Target: lis.Addr().String(), TimeoutMs: 2000})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !open.GetOk() || open.GetError() != "" {
		t.Fatalf("open port must probe ok, got %+v", open)
	}

	// Grab a port that is certainly closed by binding and immediately closing.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	_ = dead.Close()

	closed, err := srv.Probe(ctx, &genv1.ProbeRequest{Target: deadAddr, TimeoutMs: 1000})
	if err != nil {
		t.Fatalf("probe must not return a transport error for a closed port: %v", err)
	}
	if closed.GetOk() || closed.GetError() == "" {
		t.Fatalf("closed port must report a failure reason, got %+v", closed)
	}

	if _, err := srv.Probe(ctx, &genv1.ProbeRequest{}); err != nil {
		t.Fatalf("empty target: %v", err)
	}
	empty, _ := srv.Probe(ctx, &genv1.ProbeRequest{})
	if empty.GetOk() || !strings.Contains(empty.GetError(), "required") {
		t.Fatalf("empty target must be rejected, got %+v", empty)
	}

	// A hung target must not hold the agent past its capped timeout.
	start := time.Now()
	_, _ = srv.Probe(ctx, &genv1.ProbeRequest{Target: "192.0.2.1:65000", TimeoutMs: 200})
	if elapsed := time.Since(start); elapsed > 11*time.Second {
		t.Fatalf("probe exceeded its cap: %s", elapsed)
	}
}
