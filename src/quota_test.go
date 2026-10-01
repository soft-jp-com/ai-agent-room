package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeCost(t *testing.T) {
	out := "You are currently using your subscription to power your Claude Code usage\r\n\r\n" +
		"Current session: 15% used · resets Sep 25, 2:19pm (Asia/Tokyo)\r\n" +
		"Current week (all models): 16% used · resets Sep 26, 1:59am (Asia/Tokyo)\r\n" +
		"Current week (Fable): 0% used · resets Sep 26, 2am (Asia/Tokyo)\r\n\r\n" +
		"Last 24h · 553 requests · 19 sessions\r\n  83% of your usage was at >150k context\r\n"
	ws := parseClaudeCost(out)
	if len(ws) != 3 {
		t.Fatalf("windows = %d, want 3: %+v", len(ws), ws)
	}
	if ws[0].Label != "Current session" || ws[0].UsedPercent != 15 || ws[0].Resets != "Sep 25, 2:19pm (Asia/Tokyo)" {
		t.Errorf("ws[0] = %+v", ws[0])
	}
	if ws[1].Label != "Current week (all models)" || ws[1].UsedPercent != 16 {
		t.Errorf("ws[1] = %+v", ws[1])
	}
	if ws[2].UsedPercent != 0 {
		t.Errorf("ws[2] = %+v", ws[2])
	}
	if got := parseClaudeCost("Error: something\nUsage unavailable"); len(got) != 0 {
		t.Errorf("形式が違う出力から利用枠を作ってはいけない: %+v", got)
	}
}

func TestParseCodexRateLimits(t *testing.T) {
	// 他の応答・通知は読み飛ばす
	if _, isReply, _ := parseCodexRateLimits([]byte(`{"id":1,"result":{"userAgent":"x"}}`)); isReply {
		t.Error("id=1 の応答を rateLimits の応答として扱ってはいけない")
	}
	if _, isReply, _ := parseCodexRateLimits([]byte(`{"method":"notice","params":{}}`)); isReply {
		t.Error("通知を応答として扱ってはいけない")
	}
	line := `{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":91,"windowDurationMins":300,"resetsAt":1790313798},` +
		`"secondary":{"usedPercent":14,"windowDurationMins":10080,"resetsAt":1790900598},"credits":{"hasCredits":false}}}}`
	ws, isReply, err := parseCodexRateLimits([]byte(line))
	if !isReply || err != nil || len(ws) != 2 {
		t.Fatalf("ws=%+v isReply=%v err=%v", ws, isReply, err)
	}
	if ws[0].Label != "5時間枠" || ws[0].UsedPercent != 91 || ws[0].Resets == "" {
		t.Errorf("ws[0] = %+v", ws[0])
	}
	if ws[1].Label != "週枠" || ws[1].UsedPercent != 14 {
		t.Errorf("ws[1] = %+v", ws[1])
	}
	// 認証エラーは失敗として返す
	_, isReply, err = parseCodexRateLimits([]byte(`{"id":2,"error":{"code":-32600,"message":"account authentication required"}}`))
	if !isReply || err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Errorf("isReply=%v err=%v", isReply, err)
	}
	// 枠が空
	if _, _, err := parseCodexRateLimits([]byte(`{"id":2,"result":{"rateLimits":{"primary":null,"secondary":null}}}`)); err == nil {
		t.Error("枠がない応答は失敗にする")
	}
}

func TestParseAgyCredits(t *testing.T) {
	c, ok := parseAgyCredits("Remaining credits\t0\r\nUpgrade\thttps://antigravity.google/g1-upgrade\r\n")
	if !ok || c != "0" {
		t.Errorf("c=%q ok=%v", c, ok)
	}
	if _, ok := parseAgyCredits("Upgrade\thttps://example.com"); ok {
		t.Error("残高の行がなければ失敗にする")
	}
}

func TestQuotaSummary(t *testing.T) {
	if got := (Quota{}).summary(); got != "不明" {
		t.Errorf("未取得 = %q", got)
	}
	if got := (Quota{Error: "boom"}).summary(); !strings.Contains(got, "取得に失敗") || !strings.Contains(got, "boom") {
		t.Errorf("前回値なしの失敗 = %q", got)
	}
	q := Quota{Windows: []QuotaWindow{{Label: "5時間枠", UsedPercent: 91, Resets: "2026-09-25 14:23"}}, Fetched: "2026-09-25 11:05:45"}
	got := q.summary()
	for _, want := range []string{"5時間枠 91%使用", "リセット 2026-09-25 14:23", "取得 2026-09-25 11:05:45"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary に %q がない: %s", want, got)
		}
	}
	q.Error = "timeout"
	if got := q.summary(); !strings.Contains(got, "古い値") || !strings.Contains(got, "91%") {
		t.Errorf("取得失敗時は前回値を残して古い値と示す: %s", got)
	}
	// Antigravity のクレジット残高は利用枠の残率と区別する
	c := Quota{Credits: "0", Fetched: "2026-09-25 11:05:45"}.summary()
	if !strings.Contains(c, "クレジット残高 0") || !strings.Contains(c, "利用枠の残率ではありません") {
		t.Errorf("credits = %s", c)
	}
}

type quotaAdapter struct {
	fakeAdapter
	q   Quota
	err error
}

func (a quotaAdapter) FetchQuota(_ context.Context, _ string) (Quota, error) { return a.q, a.err }

func TestRefreshQuotaKeepsPreviousOnFailure(t *testing.T) {
	r, a := newTestRoom(t)
	ad := quotaAdapter{q: Quota{Windows: []QuotaWindow{{Label: "週枠", UsedPercent: 14}}, Source: "test"}}
	a.Adapter = ad
	r.refreshQuota(a, true)
	if !a.quota.has() || a.quota.Fetched == "" || a.quota.Error != "" {
		t.Fatalf("成功時の保持: %+v", a.quota)
	}
	a.Adapter = quotaAdapter{err: errors.New("認証が必要です")}
	r.refreshQuota(a, true)
	if !a.quota.has() || a.quota.Windows[0].UsedPercent != 14 || a.quota.Error == "" {
		t.Fatalf("失敗しても前回値を消さず理由を添える: %+v", a.quota)
	}
	// usage.json に保存され、再起動後に読み込まれる
	r2 := NewRoom([]*Agent{{ID: a.ID, Name: "X", Adapter: ad}}, r.workdir, r.logDir, 10, time.Second, r.log)
	if q := r2.agents[0].quota; !q.has() || q.Error == "" {
		t.Errorf("再読み込み: %+v", q)
	}
	// 直近に試行済みなら force=false では取得しない
	a.Adapter = quotaAdapter{q: Quota{Windows: []QuotaWindow{{Label: "x", UsedPercent: 1}}}}
	r.refreshQuota(a, false)
	if a.quota.Windows[0].UsedPercent != 14 {
		t.Errorf("最小間隔内は再取得しない: %+v", a.quota)
	}
}

// TestLiveQuota は実際の CLI から利用枠を取得する。ログイン済みの環境で AI_AGENT_ROOM_LIVE=1 のときだけ実行する
func TestLiveQuota(t *testing.T) {
	if os.Getenv("AI_AGENT_ROOM_LIVE") != "1" {
		t.Skip("AI_AGENT_ROOM_LIVE=1 のときだけ実行する")
	}
	for name, ad := range map[string]Adapter{"claude": newClaudeAdapter(), "codex": newCodexAdapter(), "agy": newAgyAdapter()} {
		f, ok := ad.(QuotaFetcher)
		if !ok || !ad.Available() {
			t.Logf("%s: 対象外または未インストール", name)
			continue
		}
		q, err := f.FetchQuota(context.Background(), t.TempDir())
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		q.Fetched = "now"
		t.Logf("%s: %s", name, q.summary())
	}
}

// countingQuotaAdapter は利用枠の取得回数を数える
type countingQuotaAdapter struct {
	fakeAdapter
	calls chan string
	id    string
}

func (a countingQuotaAdapter) FetchQuota(_ context.Context, _ string) (Quota, error) {
	a.calls <- a.id
	return Quota{Credits: "1", Source: "test"}, nil
}

// 起動時の取得では一時停止中のエージェントの利用枠を取得せず、再開したら取り直す
func TestRefreshQuotasSkipsPaused(t *testing.T) {
	calls := make(chan string, 10)
	on := &Agent{ID: "on", Name: "On", Adapter: countingQuotaAdapter{calls: calls, id: "on"}}
	off := &Agent{ID: "off", Name: "Off", Adapter: countingQuotaAdapter{calls: calls, id: "off"}, paused: true}
	r := NewRoom([]*Agent{on, off}, t.TempDir(), t.TempDir(), 10, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r.refreshQuotas(true)
	if got := <-calls; got != "on" {
		t.Fatalf("取得したのは %q", got)
	}
	select {
	case got := <-calls:
		t.Fatalf("一時停止中の %q を取得した", got)
	case <-time.After(300 * time.Millisecond):
	}

	if err := r.SetPaused("off", false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-calls:
		if got != "off" {
			t.Fatalf("再開後に取得したのは %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("再開しても取得しない")
	}
}
