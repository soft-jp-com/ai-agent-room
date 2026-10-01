package main

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUISmoke はヘッドレス Edge でチャット、設定ダイアログ、CLI 出力画面を開き、
// スクリーンショットと JavaScript エラーを確認する。AI_AGENT_ROOM_LIVE=1 のときだけ動く。
// 実行例: AI_AGENT_ROOM_LIVE=1 go test -run TestUISmoke -v
func TestUISmoke(t *testing.T) {
	if os.Getenv("AI_AGENT_ROOM_LIVE") != "1" {
		t.Skip("AI_AGENT_ROOM_LIVE=1 のときだけ実行")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js が見つかりません")
	}
	edge := findEdgeForUISmoke()
	if edge == "" {
		t.Skip("Microsoft Edge が見つかりません（AI_AGENT_ROOM_EDGE で指定できます）")
	}

	src, err := os.Getwd() // go test はパッケージのフォルダ（src）で実行される
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(src)
	logDir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	a := &Agent{ID: "codex", Type: "codex", Name: "Codex", Color: "#4da3ff", Adapter: uiSmokeAdapter{}}
	r := NewRoom([]*Agent{a}, workdir, logDir, 10, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	prev := protection
	setProtection(Protection{}) // 共通の目印（localappdata など）だけで判定する
	t.Cleanup(func() { setProtection(prev) })
	r.mu.Lock()
	r.postLocked("human", "画面確認用の発言", "chat")
	// @human 宛ての発言は枠とバッジで目立たせ、未完了の件数を上に出す（案 7.2 の第一段階）
	r.postLocked("codex", "@human 画面確認用の判断のお願いです", "chat")
	// 「@human 宛て以外を隠す」で隠れる発言
	r.postLocked("codex", "画面確認用のエージェント同士の発言", "chat")
	// シェルのブロックには［実行］が付き、json のブロックには付かないことを確かめる（案 7.1）
	r.postLocked("codex", "実行ボタン確認用\n```powershell\nWrite-Output smoke\n```\n```json\n{}\n```", "chat")
	// 保護するパスに触れるブロックは警告色にし、実行前に必ず確認する（blocks[].protected）
	r.postLocked("codex", "保護パス確認用\n```powershell\nGet-Content $env:LOCALAPPDATA\\ai-agent-room\\config\\rules.md\n```", "chat")
	// トークン使用量（案15）: 左ペインのエージェント欄に1行で出し、取得できなかった実行の回数も出す
	a.usage.add(Usage{Known: true, Input: 1500000, CachedInput: 1200000, Output: 3000})
	a.usage.add(Usage{})
	a.lastInput = 250000
	r.mu.Unlock()
	r.liveFor(a.ID).add("info", "画面確認用の CLI 出力")

	mux := http.NewServeMux()
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { serveUISmokePage(w, "index.html") })
	mux.HandleFunc("GET /__ui_smoke_wait", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond) // 画面の load を遅らせ、SSE の最初のデータを描画させる
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /live.html", func(w http.ResponseWriter, _ *http.Request) { serveUISmokePage(w, "live.html") })
	mux.HandleFunc("GET /api/events", r.handleEvents)
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, _ *http.Request) { writeData(w, http.StatusOK, r.Models()) })
	mux.HandleFunc("GET /api/agents/{id}/live", r.handleLive)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	token, err := loadOrCreateAuthToken(logDir, t.TempDir())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &http.Server{Handler: localOnly(port, requireAuth(port, token, mux))}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	artifacts := os.Getenv("AI_AGENT_ROOM_UI_ARTIFACT_DIR")
	if artifacts == "" {
		artifacts = filepath.Join(os.TempDir(), "ai-agent-room-ui-smoke")
	}
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(root, "development", "ui-smoke.mjs"), edge,
		"http://127.0.0.1:"+strconv.Itoa(port)+"/?token="+token, artifacts)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if ctx.Err() != nil {
		t.Fatalf("UI smoke test timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("UI smoke test failed: %v", err)
	}
}

func serveUISmokePage(w http.ResponseWriter, name string) {
	b, err := webFS.ReadFile("web/" + name)
	if err != nil {
		http.Error(w, "test page not found", http.StatusNotFound)
		return
	}
	const captureErrors = `<script>
window.__uiSmokeErrors = [];
window.addEventListener('error', (e) => { if (e instanceof ErrorEvent) window.__uiSmokeErrors.push(e.message || String(e.error)); });
window.addEventListener('unhandledrejection', (e) => window.__uiSmokeErrors.push(String(e.reason)));
</script>`
	const reportView = `<script>
// --dump-dom は load の直後に DOM を出すので、確認は load の中で同期的に行う。load は下の img（サーバが遅らせて返す）を
// 待つので、その間に SSE の最初のデータが届いて描画される。--virtual-time-budget は SSE の接続が開いたままだと終わらないので使わない
window.addEventListener('load', () => {
  const mode = new URLSearchParams(location.search).get('smoke');
  if (mode === 'settings') document.querySelector('#settings')?.showModal();
  {
    let check = {};
    if (mode === 'chat') {
      const header = document.querySelector('header')?.getBoundingClientRect();
      const pane = document.querySelector('#pane')?.getBoundingClientRect();
      check = {
        twoPane: getComputedStyle(document.body).flexDirection === 'row' && header?.width >= 200 && pane?.width >= 700,
        sample: document.querySelector('#list')?.innerText.includes('画面確認用の発言') === true,
      };
      // ［実行］を押すと、発言IDとブロック番号だけを実行 API に送る。進行役以外の発言なので確認を挟み、confirm: true を送る。
      // fetch と confirm は差し替え、実際には実行しない
      const run = document.querySelector('#list .cmd [data-cmd="run"]');
      let req = null;
      if (run) {
        const fetch0 = window.fetch, confirm0 = window.confirm;
        window.confirm = () => true;
        window.fetch = (url, opt) => { req = { url: String(url), body: JSON.parse(opt.body) }; return new Promise(() => {}); };
        run.click();
        window.fetch = fetch0; window.confirm = confirm0;
      }
      // @human 宛ての発言: 枠とバッジが付き、未完了の件数に数えられる。［済み］を押すと件数から外れる
      const toHuman = document.querySelectorAll('#list .msg.tohuman');
      const inbox = document.querySelector('#inbox');
      check.humanFrame = toHuman.length === 1 && toHuman[0].querySelector('.badge')?.textContent.includes('確認・判断') === true;
      check.inboxShown = inbox?.classList.contains('show') === true && inbox.dataset.open === '1';
      toHuman[0]?.querySelector('[data-todo]')?.click();
      // 実行待ちのコマンド（案11）が残っていれば、依頼を済みにしても件数の表示は残る
      check.humanDone = toHuman[0]?.classList.contains('done') === true && inbox?.dataset.open === '0' &&
        inbox.classList.contains('show') === (inbox.dataset.pending !== '0');
      toHuman[0]?.querySelector('[data-todo]')?.click(); // 未完了に戻す
      // 受信箱の一覧: 件数を押すと開き、一覧の［済み］で片付けられる
      inbox?.click();
      const panel = document.querySelector('#inboxPanel');
      check.inboxPanel = panel?.hidden === false && panel.querySelectorAll('#inboxList .iitem').length === 1 &&
        panel.querySelector('#inboxList .iitem .itext')?.textContent.includes('判断のお願い') === true;
      // 実行待ちのコマンド（案11）: 期限内でまだ実行していないブロックが、件数と一覧に出る
      const pendingRows = panel?.querySelectorAll('#cmdList .iitem') || [];
      check.pendingCmds = pendingRows.length === document.querySelectorAll('#list .cmd').length && pendingRows.length > 0 &&
        inbox?.dataset.pending === String(pendingRows.length) && !!pendingRows[0].querySelector('[data-crun]');
      panel?.querySelector('#inboxList .iitem [data-idone]')?.click();
      check.inboxPanelDone = toHuman[0]?.classList.contains('done') === true && inbox?.dataset.open === '0' &&
        panel?.querySelectorAll('#inboxList .iitem').length === 0;
      toHuman[0]?.querySelector('[data-todo]')?.click(); // スクリーンショットでは、一覧を開いたまま未完了の見た目を残す
      // 「@human 宛て以外を隠す」: エージェント同士の発言だけが隠れ、件数が出る。外すと戻る
      const hide = document.querySelector('#hideMinor');
      const minor = [...document.querySelectorAll('#list .msg')].find((el) => el.innerText.includes('エージェント同士の発言'));
      const shown = (el) => !!el && el.offsetParent !== null;
      if (hide) { hide.checked = true; hide.dispatchEvent(new Event('change')); }
      check.hideMinor = !shown(minor) && shown(toHuman[0]) && shown(run?.closest('.msg')) &&
        [...document.querySelectorAll('#list .msg.human')].every(shown) && document.querySelector('#hiddenCount')?.textContent.includes('1') === true;
      if (hide) { hide.checked = false; hide.dispatchEvent(new Event('change')); }
      check.showAll = shown(minor) && document.querySelector('#hiddenCount')?.textContent === '';
      const cmds = document.querySelectorAll('#list .cmd');
      check.runButton = !!run && cmds.length === 2;
      // 保護するパスに触れるブロックは警告色になり、確認が必ず出る。禁止ルールを渡せないエージェント（テスト用）も確認が必ず出る
      check.protectedBlock = cmds[1]?.classList.contains('danger') === true && cmds[1].dataset.force === '1' &&
        cmds[1].querySelector('.cmdnote')?.textContent.includes('保護している') === true;
      check.unenforced = cmds[0]?.dataset.force === '1' && !cmds[0].classList.contains('danger');
      check.runRequest = !!req && /^\/api\/messages\/\d+\/blocks\/0\/run$/.test(req.url) && req.body.confirm === true &&
        typeof req.body.cwd === 'string' && !('code' in req.body) && run.disabled === true;
      const usage = document.querySelector('#agents .chip .usage');
      check.usage = usage?.textContent.includes('250.0k') === true && usage.textContent.includes('1.5M') === true &&
        usage.textContent.includes('取得できず 1回') === true && usage.title.includes('1,500,000') === true;
    } else if (mode === 'settings') {
      check = { settingsOpen: document.querySelector('#settings')?.open === true };
    } else if (mode === 'live') {
      check = { liveOutput: document.querySelector('#out')?.textContent.includes('画面確認用の CLI 出力') === true };
    }
    document.documentElement.setAttribute('data-ui-smoke-ready', mode || '');
    document.documentElement.setAttribute('data-ui-smoke-errors', encodeURIComponent(window.__uiSmokeErrors.join('\n')));
    document.documentElement.setAttribute('data-ui-smoke-check', encodeURIComponent(JSON.stringify(check)));
  }
});
</script><img src="/__ui_smoke_wait" alt="" hidden>`
	html := strings.Replace(string(b), "<head>", "<head>"+captureErrors, 1)
	html = strings.Replace(html, "</body>", reportView+"</body>", 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, html)
}

func findEdgeForUISmoke() string {
	if p := os.Getenv("AI_AGENT_ROOM_EDGE"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("msedge"); err == nil {
		return p
	}
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

type uiSmokeAdapter struct{}

func (uiSmokeAdapter) Available() bool       { return true }
func (uiSmokeAdapter) Models() []ModelOption { return nil }
func (uiSmokeAdapter) ModelSource() string   { return "test" }
func (uiSmokeAdapter) SelfTool() bool        { return false }
func (uiSmokeAdapter) Run(context.Context, string, string, string, string) (TurnResult, error) {
	return TurnResult{Text: "画面確認用の返答"}, nil
}
