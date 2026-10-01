package main

// CLI の診断（強化案 6）。人間が設定ダイアログの［診断］を押したときだけ、各エージェントの CLI を --version で起動し、
// 見つからない・起動に失敗した・時間切れ・取得できた（バージョン）を区別して返す。起動時には自動で実行しない。
// 起動は runProcess を使うので、窓を出さず（hideWindow）、時間切れならプロセスツリーごと終了させる。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// diagTimeout は1つの CLI の --version を待つ上限。テストでは差し替える
var diagTimeout = 5 * time.Second

// DiagResult はエージェント1つ分の診断結果
type DiagResult struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Status  string `json:"status"`            // ok / not_found / failed / timeout / unsupported
	Version string `json:"version,omitempty"` // status が ok のとき、出力の最初の行
	Detail  string `json:"detail,omitempty"`  // 失敗の理由（エラー出力の最初の行など）
	Bin     string `json:"bin,omitempty"`     // 起動した実行ファイル
	Ms      int64  `json:"ms"`                // かかった時間
}

// versionCommander は --version を実行するコマンドを返せるアダプタ。bin が空なら CLI が見つからない
type versionCommander interface {
	versionCommand() (bin string, args []string)
}

func (a *claudeAdapter) versionCommand() (string, []string) { return a.bin, []string{"--version"} }
func (a *codexAdapter) versionCommand() (string, []string) {
	return a.bin, append(append([]string{}, a.pre...), "--version")
}
func (a *agyAdapter) versionCommand() (string, []string) { return a.bin, []string{"--version"} }

// Diagnose は全エージェントの CLI を並行して診断する
func (r *Room) Diagnose(ctx context.Context) []DiagResult {
	agents := r.agentsSnapshot()
	r.mu.Lock()
	cwd := r.workdir
	r.mu.Unlock()
	res := make([]DiagResult, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = diagnoseAgent(ctx, a, cwd)
		}()
	}
	wg.Wait()
	for _, d := range res {
		r.log.Info("diag", "agent", d.Agent, "status", d.Status, "version", d.Version, "detail", d.Detail, "latency_ms", d.Ms)
	}
	return res
}

func diagnoseAgent(ctx context.Context, a *Agent, cwd string) DiagResult {
	d := DiagResult{Agent: a.ID, Name: a.Name}
	vc, ok := a.Adapter.(versionCommander)
	if !ok {
		d.Status = "unsupported"
		return d
	}
	bin, args := vc.versionCommand()
	if bin == "" {
		d.Status, d.Detail = "not_found", "実行ファイルが見つかりません（PATH または *_BIN を確認してください）"
		return d
	}
	d.Bin = bin
	start := time.Now()
	out, err := runProcess(withTurnTimeout(ctx, diagTimeout), bin, args, "", cwd)
	d.Ms = time.Since(start).Milliseconds()
	var ae *AgentError
	switch {
	case errors.As(err, &ae) && ae.Code == ErrAgentTimeout:
		d.Status, d.Detail = "timeout", ae.Msg
	case err != nil:
		d.Status, d.Detail = "failed", err.Error()
	case out.exitCode != 0:
		d.Status, d.Detail = "failed", firstLine(out.stderr, out.stdout)
	default:
		d.Status, d.Version = "ok", firstLine(out.stdout, out.stderr)
	}
	return d
}

// firstLine は最初の空でない出力の、最初の空でない行を返す（長ければ切る）
func firstLine(outs ...string) string {
	for _, s := range outs {
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				if r := []rune(l); len(r) > 200 {
					l = string(r[:200]) + "…"
				}
				return l
			}
		}
	}
	return ""
}

// handleDiag は各 CLI を診断して結果を返す（人間が［診断］を押したときだけ呼ばれる）
func (room *Room) handleDiag(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, room.Diagnose(r.Context()))
}
