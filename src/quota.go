package main

// プランの残り利用枠の取得と保持。CLI ごとに取得手段が異なるため、取得できた項目だけを保持する。
//   - Claude Code: `claude -p "/cost"` の標準出力（人間向けテキスト）をパースする
//   - Codex CLI:   `codex app-server` の JSON-RPC `account/rateLimits/read`
//   - Antigravity: `agy -p "/credits"` の標準出力（クレジット残高のみ。利用枠の使用率は取得できない）
// 取得に失敗しても前回値は消さず、失敗理由を添えて「古い情報」として残す。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	quotaTimeout     = 60 * time.Second // 1回の取得の上限
	quotaMinInterval = 60 * time.Second // ジョブ終了時の再取得の最小間隔
)

// QuotaWindow は利用枠の1区間（例: 5時間枠・週枠）
type QuotaWindow struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	Resets      string  `json:"resets,omitempty"` // リセット時刻（CLI が示した表記。Claude は原文のまま、Codex はローカル時刻）
}

// Quota は取得した利用枠。取得できなかった項目は空のまま
type Quota struct {
	Windows []QuotaWindow `json:"windows,omitempty"`
	Credits string        `json:"credits,omitempty"` // 追加クレジットの残高（Antigravity）。利用枠の残率ではない
	Source  string        `json:"source,omitempty"`  // 取得元（CLI・コマンド）
	Fetched string        `json:"fetched,omitempty"` // 最後に取得できた時刻（ローカル時刻）
	Error   string        `json:"error,omitempty"`   // 直近の取得失敗の理由（成功したら空）
	Tried   string        `json:"tried,omitempty"`   // 直近に取得を試みた時刻
}

func (q Quota) has() bool { return len(q.Windows) > 0 || q.Credits != "" }

// summary はプロンプトと画面に示す文面。古い値・取得失敗を区別できるようにする
func (q Quota) summary() string {
	if !q.has() {
		if q.Error != "" {
			return "不明（取得に失敗: " + q.Error + "）"
		}
		return "不明"
	}
	var parts []string
	for _, w := range q.Windows {
		s := fmt.Sprintf("%s %.0f%%使用", w.Label, w.UsedPercent)
		if w.Resets != "" {
			s += "（リセット " + w.Resets + "）"
		}
		parts = append(parts, s)
	}
	if q.Credits != "" {
		parts = append(parts, "クレジット残高 "+q.Credits+"（追加クレジットの残高で、通常の利用枠の残率ではありません）")
	}
	s := strings.Join(parts, " / ") + "。取得 " + q.Fetched
	if q.Error != "" {
		s += "。最新の取得に失敗したため古い値です（" + q.Error + "）"
	}
	return s
}

// QuotaFetcher はプランの残り利用枠を取得できるアダプタ
type QuotaFetcher interface {
	FetchQuota(ctx context.Context, cwd string) (Quota, error)
}

// ---- Claude Code ------------------------------------------------------------

func (a *claudeAdapter) FetchQuota(ctx context.Context, cwd string) (Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	r, err := runProcess(ctx, a.bin, []string{"-p", "/cost"}, "", cwd)
	if err != nil {
		return Quota{}, err
	}
	if r.exitCode != 0 {
		return Quota{}, procFailure("claude", r)
	}
	ws := parseClaudeCost(r.stdout)
	if len(ws) == 0 {
		return Quota{}, errors.New("/cost の出力に利用枠の行が見つかりません（形式が変わった可能性があります）")
	}
	return Quota{Windows: ws, Source: `claude -p "/cost"`}, nil
}

// 例: "Current session: 15% used · resets Sep 25, 2:19pm (Asia/Tokyo)"
var claudeCostRe = regexp.MustCompile(`^\s*(.+?):\s*(\d+(?:\.\d+)?)%\s*used(?:\s*[·•-]\s*resets\s+(.+?))?\s*$`)

func parseClaudeCost(out string) []QuotaWindow {
	var ws []QuotaWindow
	for _, line := range strings.Split(out, "\n") {
		m := claudeCostRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		var pct float64
		fmt.Sscanf(m[2], "%f", &pct)
		ws = append(ws, QuotaWindow{Label: m[1], UsedPercent: pct, Resets: m[3]})
	}
	return ws
}

// ---- Codex CLI --------------------------------------------------------------

type codexRateWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins int     `json:"windowDurationMins"`
	ResetsAt           int64   `json:"resetsAt"`
}

type codexRateLimits struct {
	Primary   *codexRateWindow `json:"primary"`
	Secondary *codexRateWindow `json:"secondary"`
}

func (a *codexAdapter) FetchQuota(ctx context.Context, cwd string) (Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	args := append(append([]string{}, a.pre...), "app-server")
	cmd := exec.Command(a.bin, args...)
	cmd.Dir = cwd
	hideWindow(cmd)
	newProcessGroup(cmd) // 時間切れの killTree で子プロセスまで終了させる（Windows 以外）
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Quota{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Quota{}, err
	}
	if err := cmd.Start(); err != nil {
		return Quota{}, &AgentError{ErrAgentUnavailable, fmt.Sprintf("codex app-server を起動できません: %v", err)}
	}
	done := make(chan struct{})
	defer func() { // 取得が済んだら（タイムアウト時も）子プロセスを残さない
		close(done)
		_ = stdin.Close()
		killTree(cmd)
		_ = cmd.Wait()
	}()

	type reply struct {
		ws  []QuotaWindow
		err error
	}
	ch := make(chan reply, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			ws, isReply, err := parseCodexRateLimits(sc.Bytes())
			if isReply {
				ch <- reply{ws, err}
				return
			}
		}
		select {
		case ch <- reply{err: errors.New("応答を受け取る前に app-server が終了しました")}:
		case <-done:
		}
	}()

	// initialize → initialized → account/rateLimits/read の順に送る
	for _, m := range []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"ai_agent_room","title":"AI Agent Room","version":"1.0"}}}`,
		`{"method":"initialized","params":{}}`,
		`{"id":2,"method":"account/rateLimits/read"}`,
	} {
		if _, err := io.WriteString(stdin, m+"\n"); err != nil {
			return Quota{}, err
		}
	}
	select {
	case rp := <-ch:
		if rp.err != nil {
			return Quota{}, rp.err
		}
		return Quota{Windows: rp.ws, Source: "codex app-server account/rateLimits/read"}, nil
	case <-ctx.Done():
		return Quota{}, &AgentError{ErrAgentTimeout, fmt.Sprintf("%v 以内に応答がありませんでした", quotaTimeout)}
	}
}

// parseCodexRateLimits は app-server の1行を解釈する。id=2（rateLimits の応答）なら isReply=true
func parseCodexRateLimits(line []byte) (ws []QuotaWindow, isReply bool, err error) {
	var m struct {
		ID     *int `json:"id"`
		Result *struct {
			RateLimits *codexRateLimits `json:"rateLimits"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil || m.ID == nil || *m.ID != 2 {
		return nil, false, nil
	}
	if m.Error != nil {
		return nil, true, errors.New(m.Error.Message)
	}
	if m.Result == nil || m.Result.RateLimits == nil {
		return nil, true, errors.New("応答に rateLimits がありません")
	}
	for _, w := range []*codexRateWindow{m.Result.RateLimits.Primary, m.Result.RateLimits.Secondary} {
		if w == nil {
			continue
		}
		qw := QuotaWindow{Label: codexWindowLabel(w.WindowDurationMins), UsedPercent: w.UsedPercent}
		if w.ResetsAt > 0 {
			qw.Resets = time.Unix(w.ResetsAt, 0).Format("2006-01-02 15:04")
		}
		ws = append(ws, qw)
	}
	if len(ws) == 0 {
		return nil, true, errors.New("応答に利用枠がありません")
	}
	return ws, true, nil
}

func codexWindowLabel(mins int) string {
	switch {
	case mins == 300:
		return "5時間枠"
	case mins == 10080:
		return "週枠"
	case mins > 0 && mins%1440 == 0:
		return fmt.Sprintf("%d日枠", mins/1440)
	case mins > 0 && mins%60 == 0:
		return fmt.Sprintf("%d時間枠", mins/60)
	case mins > 0:
		return fmt.Sprintf("%d分枠", mins)
	}
	return "利用枠"
}

// ---- Antigravity ------------------------------------------------------------

func (a *agyAdapter) FetchQuota(ctx context.Context, cwd string) (Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, quotaTimeout)
	defer cancel()
	r, err := runProcess(ctx, a.bin, []string{"-p", "/credits", "--print-timeout", "50s"}, "", cwd)
	if err != nil {
		return Quota{}, err
	}
	if r.exitCode != 0 {
		return Quota{}, procFailure("agy", r)
	}
	c, ok := parseAgyCredits(r.stdout)
	if !ok {
		return Quota{}, errors.New("/credits の出力にクレジット残高が見つかりません（形式が変わった可能性があります）")
	}
	return Quota{Credits: c, Source: `agy -p "/credits"`}, nil
}

// 例: "Remaining credits\t0"（区切りはタブまたは空白）
var agyCreditsRe = regexp.MustCompile(`(?im)^\s*Remaining credits\s*[:\t ]\s*(\S.*?)\s*$`)

func parseAgyCredits(out string) (string, bool) {
	m := agyCreditsRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ---- Room への組み込み ---------------------------------------------------------

// refreshQuota はエージェント1つの利用枠を取得して保持する。CLI の起動に時間がかかるため、ロックの外で実行する。
// force=false のときは、直近に取得を試みてから quotaMinInterval 以内なら何もしない。
func (r *Room) refreshQuota(a *Agent, force bool) {
	f, ok := a.Adapter.(QuotaFetcher)
	if !ok || !a.Adapter.Available() {
		return
	}
	r.mu.Lock()
	if a.quotaBusy || (!force && time.Since(a.quotaTried) < quotaMinInterval) {
		r.mu.Unlock()
		return
	}
	a.quotaBusy, a.quotaTried = true, time.Now()
	workdir := r.workdir
	r.mu.Unlock()

	q, err := f.FetchQuota(context.Background(), workdir)

	r.mu.Lock()
	defer r.mu.Unlock()
	a.quotaBusy = false
	now := time.Now().Format("2006-01-02 15:04:05")
	prev := a.quota
	if err != nil {
		prev.Error, prev.Tried = err.Error(), now // 前回値は消さない
		a.quota = prev
		r.log.Warn("quota.fetch", "agent", a.ID, "ok", false, "error", err.Error(), "stale", prev.has())
	} else {
		q.Fetched, q.Tried = now, now
		a.quota = q
		r.log.Info("quota.fetch", "agent", a.ID, "ok", true, "source", q.Source, "windows", len(q.Windows), "credits", q.Credits)
	}
	r.saveUsageLocked()
	r.pushStatusLocked()
}

// refreshQuotas は起動時に、全エージェントの利用枠を並行して取得する。定期的な取得はしない（2026-10-01 人間の判断）。
// 利用枠はターンの終了時・再開時・エージェントの追加時にも取り直すので、動いているエージェントの値は新しいまま保たれる。
// 一時停止中のエージェント（利用枠の上限で止まったものを含む）は取得しない。CLI の起動を減らすため
// （Antigravity は起動のたびに自動更新のプロセスを切り離して起動し、その子が窓を一瞬出すことがある）
func (r *Room) refreshQuotas(force bool) {
	for _, a := range r.agentsSnapshot() {
		r.mu.Lock()
		paused := a.paused
		r.mu.Unlock()
		if paused {
			r.log.Debug("quota.fetch", "agent", a.ID, "skipped", "paused")
			continue
		}
		go r.refreshQuota(a, force)
	}
}
