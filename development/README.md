# 開発ガイド

AI Agent Room を開発するときに知っておくこと。機能と使い方は `README.ja.md`（英語版は `README.md`）、詳しい仕様は `docs/spec.md` を参照。

## コマンド

```powershell
go build -C src -o ..\ai-agent-room.exe .   # ビルド
go vet -C src ./...                   # 静的検査
go test -C src ./...                  # 単体テスト
.\ai-agent-room.exe -workdir C:\path\to\project -no-open   # 起動（URL はコンソールに表示）

# 画面の自動確認（Node.js と Edge が必要。スクリーンショットを保存する）
$env:AI_AGENT_ROOM_LIVE = "1"; go test -C src -run TestUISmoke -v
```

変更したら少なくとも `go vet` と `go test` を通す。画面（`src/web/`）を変えたときは `TestUISmoke` も実行し、スクリーンショットで見た目を確かめる。

## 構成

- `src/*.go`: サーバー（`package main` 1つ）。HTTP・SSE は `main.go`、会話の進行は `chat.go`（チャット・ディスカッション）と `freetalk.go`、CLI ごとの起動と出力の解釈は `adapters.go`
- `src/web/`: 画面。ビルド工程のない素の HTML / JavaScript（`index.html` に大半がある。`go:embed` でバイナリに埋め込む）
- `development/`: 開発用の手順とスクリプト（`ui-smoke.mjs` は `TestUISmoke` から呼ばれる）
- `docs/`: 仕様書（`spec.md`）
- `logs/`: 実行時に作られるログと状態（Git 管理外）

## 実装のきまり

- **API の形**: 成功は `{"data": ...}`、失敗は `{"error_code", "message", "request_id"}`。画面と保存済みのログがこの形に依存している。エンドポイントを足したり変えたりしたら `README.md`・`README.ja.md` の API 表も直す
- **ログ**: 新しい処理には `logs/server.log` のイベント（`agent.turn.start` のような `対象.動作` 形式、`error_code` と `latency_ms` 付き）を出す。不具合の調査はこのログに頼っている
- **セキュリティ**: 待ち受けは `127.0.0.1` のみ、Host / Origin の検証、トークン認証（`auth.go`）を外したり緩めたりしない。秘密情報らしいファイルは差分に写さない（`filediff.go`）
- **対象 OS**: 主な対象は Windows 10 / 11。macOS・Linux は試験的な対応で、OS 依存の処理は `*_windows.go` / `*_unix.go` に分ける
- **画面の文言**: 文言を足したら `src/web/i18n.js` に英訳も足す
- **改行と文字コード**: ファイルは UTF-8。`start.bat` は CRLF・ASCII のみ、`start.sh` は LF（`.gitattributes` で固定）

## 不具合の調査

手順は `development/debugging.md`。
