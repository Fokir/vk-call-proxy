package tunnel

import (
	"testing"
	"time"
)

func TestDeduplicateTokens(t *testing.T) {
	tests := []struct {
		in  []string
		out int
	}{
		{[]string{"a", "b", "c"}, 3},
		{[]string{"a", "a", "b"}, 2},
		{[]string{"", "a", ""}, 1},
		{nil, 0},
		{[]string{""}, 0},
		{[]string{"x", "y", "x", "y"}, 2},
	}
	for _, tt := range tests {
		got := deduplicateTokens(tt.in)
		if len(got) != tt.out {
			t.Errorf("deduplicateTokens(%v) = %d, want %d", tt.in, len(got), tt.out)
		}
	}
}

func TestSlotState_String(t *testing.T) {
	states := map[SlotState]string{
		SlotIdle:         "idle",
		SlotConnecting:   "connecting",
		SlotAllocating:   "allocating",
		SlotActive:       "active",
		SlotReconnecting: "reconnecting",
		SlotDead:         "dead",
		SlotState(99):    "unknown",
	}
	for state, expected := range states {
		if state.String() != expected {
			t.Errorf("%d.String() = %q, want %q", state, state.String(), expected)
		}
	}
}

func TestReconnectBackoff_Linear(t *testing.T) {
	// Spec: linear backoff 3s, 6s, 9s, 12s... cap 60s
	delay := reconnectInitDelay
	expected := []time.Duration{3, 6, 9, 12, 15, 18}
	for i, exp := range expected {
		exp *= time.Second
		if delay != exp {
			t.Errorf("step %d: delay=%v, want %v", i, delay, exp)
		}
		delay = min(delay+reconnectInitDelay, reconnectMaxDelay)
	}
	// Verify cap at 60s
	delay = 59 * time.Second
	delay = min(delay+reconnectInitDelay, reconnectMaxDelay)
	if delay != reconnectMaxDelay {
		t.Errorf("cap: delay=%v, want %v", delay, reconnectMaxDelay)
	}
}

func TestGenerateNonce_Unique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		n := generateNonce()
		if len(n) != 16 { // 8 bytes = 16 hex chars
			t.Fatalf("nonce length = %d, want 16", len(n))
		}
		if seen[n] {
			t.Fatal("duplicate nonce generated")
		}
		seen[n] = true
	}
}

func TestAllocAddrs_Nil(t *testing.T) {
	addrs := allocAddrs(nil)
	if len(addrs) != 0 {
		t.Fatal("allocAddrs(nil) should return empty slice")
	}
}

func TestPoolConfig_Defaults(t *testing.T) {
	p := NewCallPool(PoolConfig{})
	if p.cfg.ConnsPerCall != 4 {
		t.Errorf("default ConnsPerCall = %d, want 4", p.cfg.ConnsPerCall)
	}
	if p.router == nil {
		t.Fatal("router should be initialized")
	}
	p.Close()
}
