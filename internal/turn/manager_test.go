package turn_test

import (
	"context"
	"log/slog"
	"net"
	"testing"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/testrig"
	"github.com/call-vpn/call-vpn/internal/turn"
)

type staticCreds struct{ c *provider.Credentials }

func (s *staticCreds) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	// Return a copy so the Manager can mutate Host/Port for round-robin
	// without affecting the original.
	cp := *s.c
	cp.Servers = make([]provider.TURNServer, len(s.c.Servers))
	copy(cp.Servers, s.c.Servers)
	return &cp, nil
}


func TestManager_Allocate(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	creds := rig.Credentials(0)

	mgr := turn.NewManager(&staticCreds{c: creds}, false, slog.Default())
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(context.Background(), 4)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(allocs) != 4 {
		t.Fatalf("expected 4 allocations, got %d", len(allocs))
	}
	for i, a := range allocs {
		if a.RelayAddr == nil {
			t.Errorf("allocation %d: RelayAddr is nil", i)
			continue
		}
		host, port, err := net.SplitHostPort(a.RelayAddr.String())
		if err != nil {
			t.Errorf("allocation %d: bad RelayAddr %q: %v", i, a.RelayAddr, err)
			continue
		}
		if host == "" || port == "" {
			t.Errorf("allocation %d: empty host or port in RelayAddr %q", i, a.RelayAddr)
		}
	}
}

func TestManager_AllocateWithCredentials(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	creds := rig.Credentials(0)

	mgr := turn.NewManager(&staticCreds{c: creds}, false, slog.Default())
	defer mgr.CloseAll()

	alloc, err := mgr.AllocateWithCredentials(context.Background(), creds)
	if err != nil {
		t.Fatalf("AllocateWithCredentials: %v", err)
	}
	if alloc.RelayAddr == nil {
		t.Fatal("RelayAddr is nil")
	}

	// Verify it appears in the allocations list.
	allocs := mgr.Allocations()
	if len(allocs) != 1 {
		t.Fatalf("expected 1 allocation, got %d", len(allocs))
	}
}

func TestManager_RoundRobin(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 2})

	creds0 := rig.Credentials(0)
	creds1 := rig.Credentials(1)

	// Verify round-robin distribution by allocating individually with
	// AllocateWithCredentials for each TURN server, then checking that
	// the Manager holds allocations from both servers.
	mgr := turn.NewManager(&staticCreds{c: creds0}, false, slog.Default())
	defer mgr.CloseAll()

	// Allocate 2 on server 0.
	for i := 0; i < 2; i++ {
		_, err := mgr.AllocateWithCredentials(context.Background(), creds0)
		if err != nil {
			t.Fatalf("allocate on server 0 (#%d): %v", i, err)
		}
	}
	// Allocate 2 on server 1.
	for i := 0; i < 2; i++ {
		_, err := mgr.AllocateWithCredentials(context.Background(), creds1)
		if err != nil {
			t.Fatalf("allocate on server 1 (#%d): %v", i, err)
		}
	}

	allocs := mgr.Allocations()
	if len(allocs) != 4 {
		t.Fatalf("expected 4 allocations, got %d", len(allocs))
	}

	// Verify allocations are distributed across different TURN server ports.
	serverPorts := make(map[string]int)
	for _, a := range allocs {
		serverPorts[a.Creds.Port]++
	}

	t.Logf("TURN server 0 port: %s, server 1 port: %s", creds0.Port, creds1.Port)
	t.Logf("allocation distribution by server port: %v", serverPorts)

	if len(serverPorts) < 2 {
		t.Errorf("expected allocations on 2 different servers, got %d", len(serverPorts))
	}
	if serverPorts[creds0.Port] != 2 || serverPorts[creds1.Port] != 2 {
		t.Errorf("expected 2 allocations per server, got server0=%d server1=%d",
			serverPorts[creds0.Port], serverPorts[creds1.Port])
	}
}

func TestManager_CloseAll(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	creds := rig.Credentials(0)

	mgr := turn.NewManager(&staticCreds{c: creds}, false, slog.Default())

	_, err := mgr.Allocate(context.Background(), 2)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if n := len(mgr.Allocations()); n != 2 {
		t.Fatalf("expected 2 allocations before CloseAll, got %d", n)
	}

	mgr.CloseAll()

	if n := len(mgr.Allocations()); n != 0 {
		t.Fatalf("expected 0 allocations after CloseAll, got %d", n)
	}
}
