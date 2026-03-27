package tunnel

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/testrig"
)

// connectPair creates two SignalingClients connected to the same room,
// waits for both to see the peer, and sets a shared encryption key.
func connectPair(t *testing.T, svc provider.Service, token string) (provider.SignalingClient, provider.SignalingClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	info1, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		t.Fatalf("FetchJoinInfo for client1: %v", err)
	}
	sig1, err := svc.ConnectSignaling(ctx, info1, logger.With("who", "sig1"))
	if err != nil {
		t.Fatalf("ConnectSignaling client1: %v", err)
	}
	t.Cleanup(func() { sig1.Close() })

	info2, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		t.Fatalf("FetchJoinInfo for client2: %v", err)
	}
	sig2, err := svc.ConnectSignaling(ctx, info2, logger.With("who", "sig2"))
	if err != nil {
		t.Fatalf("ConnectSignaling client2: %v", err)
	}
	t.Cleanup(func() { sig2.Close() })

	// Wait for sig1 to see sig2 join (mock WS sends participant-joined to existing).
	vkSig1 := sig1.(*vk.SignalingClient)
	if _, err := vkSig1.WaitForPeer(ctx, 0); err != nil {
		t.Fatalf("sig1.WaitForPeer: %v", err)
	}
	// Brief pause to ensure both sides are fully connected.
	time.Sleep(100 * time.Millisecond)

	if token != "" {
		if err := sig1.SetKey(token); err != nil {
			t.Fatalf("sig1.SetKey: %v", err)
		}
		if err := sig2.SetKey(token); err != nil {
			t.Fatalf("sig2.SetKey: %v", err)
		}
	}

	return sig1, sig2
}

func TestSignaling_RelayBatchExchange(t *testing.T) {
	rig := testrig.New(t, testrig.Options{Calls: 1})
	svc := rig.Service(0)
	token := "test-shared-secret"
	sig1, sig2 := connectPair(t, svc, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wantAddrs := []string{"1.2.3.4:5678", "5.6.7.8:9012"}
	nonce := "batch-nonce-1"

	// sig1 (client) sends a relay batch.
	if err := sig1.SendRelayBatch(ctx, wantAddrs, "client", nonce, 0, true); err != nil {
		t.Fatalf("SendRelayBatch: %v", err)
	}

	// sig2 (server) receives the batch. skipRole="" since mock WS doesn't echo.
	gotAddrs, batch, final, gotNonce, err := sig2.RecvRelayBatch(ctx, "", "")
	if err != nil {
		t.Fatalf("RecvRelayBatch: %v", err)
	}

	if gotNonce != nonce {
		t.Errorf("nonce: got %q, want %q", gotNonce, nonce)
	}
	if batch != 0 {
		t.Errorf("batch: got %d, want 0", batch)
	}
	if !final {
		t.Error("final: got false, want true")
	}
	if len(gotAddrs) != len(wantAddrs) {
		t.Fatalf("addrs length: got %d, want %d", len(gotAddrs), len(wantAddrs))
	}
	for i, a := range gotAddrs {
		if a != wantAddrs[i] {
			t.Errorf("addr[%d]: got %q, want %q", i, a, wantAddrs[i])
		}
	}
}

func TestSignaling_PunchReady(t *testing.T) {
	rig := testrig.New(t, testrig.Options{Calls: 1})
	svc := rig.Service(0)
	token := "punch-secret"
	sigA, sigB := connectPair(t, svc, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nonce := "punch-nonce-1"

	// A needs DrainAndRoute running to route custom-data to subscribers.
	drainCtx, drainCancel := context.WithCancel(ctx)
	defer drainCancel()
	go sigA.DrainAndRoute(drainCtx)

	// A starts the punch dispatcher and prepares a waiter for index 0.
	sigA.StartPunchDispatcher(ctx, nonce)
	defer sigA.StopPunchDispatcher()

	waitFn := sigA.PreparePunchWait(ctx, nonce, 0)

	// B sends punch-ready for index 0.
	if err := sigB.SendPunchReady(ctx, nonce, 0); err != nil {
		t.Fatalf("SendPunchReady: %v", err)
	}

	// A's waiter should return without error.
	if err := waitFn(); err != nil {
		t.Fatalf("PreparePunchWait returned error: %v", err)
	}
}

func TestSignaling_DisconnectHandshake(t *testing.T) {
	rig := testrig.New(t, testrig.Options{Calls: 1})
	svc := rig.Service(0)
	token := "disconnect-secret"
	sigClient, sigServer := connectPair(t, svc, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nonce := "disconnect-nonce-1"

	// Server listens for session end (disconnect request) in a goroutine.
	endCh := make(chan provider.SessionEndReason, 1)
	nonceCh := make(chan string, 1)
	go func() {
		reason, n := sigServer.WaitForSessionEnd(ctx)
		endCh <- reason
		nonceCh <- n
	}()

	// Client sends disconnect request.
	if err := sigClient.SendDisconnectReq(ctx, nonce); err != nil {
		t.Fatalf("SendDisconnectReq: %v", err)
	}

	// Server should receive the disconnect signal.
	select {
	case reason := <-endCh:
		if reason != provider.SessionEndDisconnect {
			t.Fatalf("reason: got %v, want SessionEndDisconnect", reason)
		}
		gotNonce := <-nonceCh
		if gotNonce != nonce {
			t.Errorf("nonce: got %q, want %q", gotNonce, nonce)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for disconnect signal")
	}

	// Server sends disconnect ack.
	if err := sigServer.SendDisconnectAck(ctx, nonce); err != nil {
		t.Fatalf("SendDisconnectAck: %v", err)
	}

	// Client waits for the ack. Start DrainAndRoute so Subscribe gets messages.
	drainCtx, drainCancel := context.WithCancel(ctx)
	defer drainCancel()
	go sigClient.DrainAndRoute(drainCtx)

	if err := sigClient.WaitDisconnectAck(ctx, nonce); err != nil {
		t.Fatalf("WaitDisconnectAck: %v", err)
	}
}

func TestSignaling_NonceFiltering(t *testing.T) {
	rig := testrig.New(t, testrig.Options{Calls: 1})
	svc := rig.Service(0)
	token := "nonce-filter-secret"
	sig1, sig2 := connectPair(t, svc, token)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addrsAAA := []string{"10.0.0.1:1111"}
	addrsBBB := []string{"10.0.0.2:2222"}

	// sig1 sends two batches with different nonces.
	if err := sig1.SendRelayBatch(ctx, addrsAAA, "client", "aaa", 0, true); err != nil {
		t.Fatalf("SendRelayBatch aaa: %v", err)
	}
	if err := sig1.SendRelayBatch(ctx, addrsBBB, "client", "bbb", 0, true); err != nil {
		t.Fatalf("SendRelayBatch bbb: %v", err)
	}

	// sig2 receives with filterNonce="aaa" — should only get the first batch.
	gotAddrs, _, _, gotNonce, err := sig2.RecvRelayBatch(ctx, "", "aaa")
	if err != nil {
		t.Fatalf("RecvRelayBatch with filter aaa: %v", err)
	}
	if gotNonce != "aaa" {
		t.Errorf("nonce: got %q, want %q", gotNonce, "aaa")
	}
	if len(gotAddrs) != 1 || gotAddrs[0] != addrsAAA[0] {
		t.Errorf("addrs: got %v, want %v", gotAddrs, addrsAAA)
	}

	// Now receive with filterNonce="bbb" — the "bbb" message should still be available.
	gotAddrs2, _, _, gotNonce2, err := sig2.RecvRelayBatch(ctx, "", "bbb")
	if err != nil {
		t.Fatalf("RecvRelayBatch with filter bbb: %v", err)
	}
	if gotNonce2 != "bbb" {
		t.Errorf("nonce: got %q, want %q", gotNonce2, "bbb")
	}
	if len(gotAddrs2) != 1 || gotAddrs2[0] != addrsBBB[0] {
		t.Errorf("addrs: got %v, want %v", gotAddrs2, addrsBBB)
	}
}
