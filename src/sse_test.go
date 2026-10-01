package main

import "testing"

// 詰まったクライアントは切断し、ほかのクライアントには配信を続ける（取りこぼしたまま表示が止まらないように）
func TestBroadcastDisconnectsLaggedClient(t *testing.T) {
	r, _ := newTestRoom(t)
	slow, unsubSlow := r.Subscribe()
	defer unsubSlow()
	fast, unsubFast := r.Subscribe()
	defer unsubFast()
	for len(fast) > 0 {
		<-fast
	}

	r.mu.Lock()
	for i := 0; i < cap(slow)+1; i++ {
		r.broadcastLocked(Event{Type: "partial"})
		if len(fast) > 0 {
			<-fast
		}
	}
	_, still := r.clients[slow]
	r.mu.Unlock()
	if still {
		t.Fatal("詰まったクライアントが購読から外れていない")
	}
	for range slow { // 残りを読み切ると閉じている
	}

	r.mu.Lock()
	r.broadcastLocked(Event{Type: "message"})
	r.mu.Unlock()
	if ev := <-fast; ev.Type != "message" {
		t.Fatalf("ほかのクライアントへの配信が止まった: %v", ev.Type)
	}
}
