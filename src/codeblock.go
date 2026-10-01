package main

// 発言中のコードブロックの切り出し（修正案 7.1）。
// 画面（web/index.html の render()）と同じ規則で切り出し、発言の保存時に Message.Blocks へ入れる。
// 画面はこの一覧の番号でボタンを出し、実行 API もこの番号で本文を取り直すので、両者の数え方がずれない。

import (
	"regexp"
	"strings"
)

// CodeBlock は発言中のコードブロック1つ。Lang は言語指定の最初の語を小文字にしたもの（なければ空）
type CodeBlock struct {
	Lang string `json:"lang"`
	Code string `json:"code"`
	// Protected は本文が保護するパス（AI Agent Room の設定、.claude/ など。protect.go）に触れること。画面は警告色にして必ず確認を出す
	Protected bool `json:"protected,omitempty"`
}

// codeBlockRe は render() の /```[^\n]*\n?([\s\S]*?)```/g に、言語指定の取り込みを加えたもの。
// Go の regexp は Perl と同じ優先順位で部分一致を選ぶので、JS と同じ位置で切り出される
var codeBlockRe = regexp.MustCompile("```([^\\n]*)\\n?([\\s\\S]*?)```")

// shellOf は実行できる言語指定を、実行に使うシェルの種類（pwsh / cmd / bash）に対応づける
var shellOf = map[string]string{
	"powershell": "pwsh", "pwsh": "pwsh", "ps1": "pwsh",
	"cmd": "cmd", "bat": "cmd", "batch": "cmd",
	"bash": "bash", "sh": "bash",
}

// extractCodeBlocks は text 中のコードブロックを出現順に返す。閉じていないフェンスはブロックにしない
func extractCodeBlocks(text string) []CodeBlock {
	var blocks []CodeBlock
	for _, m := range codeBlockRe.FindAllStringSubmatch(text, -1) {
		lang := ""
		if f := strings.Fields(m[1]); len(f) > 0 {
			lang = strings.ToLower(f[0])
		}
		code := strings.TrimSuffix(m[2], "\n")
		blocks = append(blocks, CodeBlock{Lang: lang, Code: code, Protected: touchesProtected(code)})
	}
	return blocks
}
