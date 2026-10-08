package main

// 変更差分の表示（案 9）: ターンの前のファイルの中身を覚えておき、ターンの間に変わったファイルの
// 差分（unified 形式）を logs/diffs/ に書き出す。システムメッセージには行数（+N -M）と差分ファイルのパスだけを載せ、
// エージェントに渡す文脈が膨らまないようにする。git がなくても使える。
//
// 中身は、パスと更新時刻・大きさが変わらない間は読み直さない（初回だけ全部読む）。
// 対象はテキストファイル（NUL を含まない）で fileDiffMaxBytes 以下のものだけ。秘密情報を含みそうな名前のファイル
// （isSecretFile）は中身を写さない（一覧には載るが行数は出ない）。logs/diffs/ は fileDiffMaxFiles 件まで残す。

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fileDiffMaxBytes   = 256 << 10 // 中身を覚えるファイルの大きさの上限
	fileDiffMaxTotal   = 64 << 20  // 覚える中身の合計の上限（超えたら新しいファイルは覚えない）
	fileDiffContext    = 3         // 差分の前後に付ける行数
	fileDiffMaxLCSCell = 4_000_000 // 最長共通部分列の表の大きさの上限（超えたら中間をまとめて置き換えとして出す）
	fileDiffMaxFiles   = 500       // logs/diffs/ に残す差分ファイルの数（超えたら古いものから消す）
)

type cachedFile struct {
	stamp fileStamp
	data  []byte // 読み込んだ後は書き換えない（ターンごとの控えと共有するため）
}

// fileContentCache はファイルの中身の控え。キーは絶対パス。ターンは並行して動くので mu で守る
type fileContentCache struct {
	mu    sync.Mutex
	files map[string]cachedFile
	total int
}

var fileContents = &fileContentCache{}

// remember は snap のうち、控えがないか古いファイルを読み込み、snap の時点の中身の控えを返す（キーは相対パス）。
// 控えを返したファイルだけが差分の対象になる。
func (c *fileContentCache) remember(workdir string, snap fileSnapshot) map[string][]byte {
	if snap == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = map[string]cachedFile{}
	}
	out := map[string][]byte{}
	for rel, st := range snap {
		if st.size > fileDiffMaxBytes || isSecretFile(rel) {
			continue
		}
		abs := filepath.Join(workdir, filepath.FromSlash(rel))
		if cf, ok := c.files[abs]; ok && cf.stamp.mod.Equal(st.mod) && cf.stamp.size == st.size {
			if cf.data != nil {
				out[rel] = cf.data
			}
			continue
		}
		if old, ok := c.files[abs]; ok {
			c.total -= len(old.data)
		}
		data, err := os.ReadFile(abs)
		if err != nil || int64(len(data)) != st.size || isBinary(data) || c.total+len(data) > fileDiffMaxTotal {
			c.files[abs] = cachedFile{stamp: st} // テキストでない・読めないものは中身なしで覚える（毎回読み直さない）
			continue
		}
		c.files[abs] = cachedFile{stamp: st, data: data}
		c.total += len(data)
		out[rel] = data
	}
	return out
}

// isSecretFile は秘密情報を含みそうなファイルか。中身を logs/diffs/ に写さないよう、差分の対象から外す
func isSecretFile(rel string) bool {
	name := strings.ToLower(filepath.Base(filepath.FromSlash(rel)))
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		return true
	}
	switch filepath.Ext(name) {
	case ".pem", ".key", ".p12", ".pfx", ".crt", ".cer", ".jks", ".keystore", ".ppk":
		return true
	}
	if strings.HasPrefix(name, "id_rsa") || strings.HasPrefix(name, "id_ed25519") || strings.HasPrefix(name, "id_ecdsa") {
		return true
	}
	for _, w := range []string{"secret", "credential", "password", "passwd", "token", "apikey", "api_key"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// fileDiff はファイル1件の差分
type fileDiff struct {
	path    string
	added   int // 追加した行数
	removed int // 削除した行数
	unified string
}

// buildFileDiffs は changes のうち、前後の中身がそろうものの差分を作る
func buildFileDiffs(changes []fileChange, before, after map[string][]byte) map[string]fileDiff {
	out := map[string]fileDiff{}
	for _, ch := range changes {
		old, okOld := before[ch.path]
		cur, okNew := after[ch.path]
		if ch.added {
			old, okOld = nil, true
		}
		if ch.deleted {
			cur, okNew = nil, true
		}
		if !okOld || !okNew {
			continue
		}
		out[ch.path] = unifiedDiff(ch.path, splitLines(old), splitLines(cur))
	}
	return out
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp struct {
	kind byte // ' ' '-' '+'
	line string
}

// diffLines は a から b への行の編集列を返す。前後の共通部分を除いた中間を最長共通部分列で比べる
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	if (len(ma)+1)*(len(mb)+1) > fileDiffMaxLCSCell {
		for _, l := range ma {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, diffOp{'+', l})
		}
	} else {
		n, m := len(ma), len(mb)
		lcs := make([]int32, (n+1)*(m+1)) // lcs[i*(m+1)+j] = ma[i:] と mb[j:] の最長共通部分列の長さ
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
				} else {
					lcs[i*(m+1)+j] = max(lcs[(i+1)*(m+1)+j], lcs[i*(m+1)+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, diffOp{' ', ma[i]})
				i++
				j++
			case i < n && (j == m || lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]): // 同じ長さなら削除を先に出す
				ops = append(ops, diffOp{'-', ma[i]})
				i++
			default:
				ops = append(ops, diffOp{'+', mb[j]})
				j++
			}
		}
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// unifiedDiff は path の差分を unified 形式で返す
func unifiedDiff(path string, a, b []string) fileDiff {
	ops := diffLines(a, b)
	d := fileDiff{path: path}
	var body strings.Builder
	// 変更行の前後 fileDiffContext 行をまとめて1つの塊にする
	for start := 0; start < len(ops); {
		for start < len(ops) && ops[start].kind == ' ' {
			start++
		}
		if start == len(ops) {
			break
		}
		from := max(0, start-fileDiffContext)
		end, lastChange := start, start
		for end < len(ops) && end-lastChange <= 2*fileDiffContext {
			if ops[end].kind != ' ' {
				lastChange = end
			}
			end++
		}
		to := min(len(ops), lastChange+1+fileDiffContext)
		// 塊の先頭の行番号を数える
		aLine, bLine := 1, 1
		for _, op := range ops[:from] {
			if op.kind != '+' {
				aLine++
			}
			if op.kind != '-' {
				bLine++
			}
		}
		var aCount, bCount int
		var hunk strings.Builder
		for _, op := range ops[from:to] {
			if op.kind != '+' {
				aCount++
			}
			if op.kind != '-' {
				bCount++
			}
			switch op.kind {
			case '+':
				d.added++
			case '-':
				d.removed++
			}
			line := op.line
			hunk.WriteByte(op.kind)
			hunk.WriteString(line)
			if !strings.HasSuffix(line, "\n") {
				hunk.WriteString("\n\\ No newline at end of file\n")
			}
		}
		if aCount == 0 {
			aLine--
		}
		if bCount == 0 {
			bLine--
		}
		fmt.Fprintf(&body, "@@ -%d,%d +%d,%d @@\n%s", aLine, aCount, bLine, bCount, hunk.String())
		start = to
	}
	if body.Len() > 0 {
		d.unified = fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", path, path, body.String())
	}
	return d
}

// writeFileDiffs は差分を logDir/diffs/ に1つのファイルとして書き出し、そのパスを返す。差分がなければ空を返す
func writeFileDiffs(logDir, agentID string, now time.Time, changes []fileChange, diffs map[string]fileDiff) (string, error) {
	var b strings.Builder
	for _, ch := range changes {
		if d, ok := diffs[ch.path]; ok && d.unified != "" {
			b.WriteString(d.unified)
		}
	}
	if b.Len() == 0 || logDir == "" {
		return "", nil
	}
	dir := filepath.Join(logDir, "diffs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("diff-%s-%s.diff", now.Format("20060102-150405"), agentID))
	if err := writeStateFile(path, []byte(b.String())); err != nil {
		return "", err
	}
	pruneFileDiffs(dir, fileDiffMaxFiles)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return path, nil
}

// pruneFileDiffs は dir の差分ファイルが keep 件を超えたら、古いもの（名前の日時が古い順）から消す
func pruneFileDiffs(dir string, keep int) {
	names, err := filepath.Glob(filepath.Join(dir, "diff-*.diff"))
	if err != nil || len(names) <= keep {
		return
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		os.Remove(n)
	}
}

// fileVersionsMessage はターンの後に、変わったファイルの一覧（案 12）と差分の行数・差分ファイルのパス（案 9）を返す。
// before・beforeData はターンの前の takeFileSnapshot と fileContents.remember の結果
func fileVersionsMessage(name, agentID, workdir, logDir string, before fileSnapshot, beforeData map[string][]byte) (string, error) {
	after := takeFileSnapshot(workdir, logDir)
	changes := diffFileSnapshots(workdir, before, after)
	if len(changes) == 0 {
		return "", nil
	}
	diffs := buildFileDiffs(changes, beforeData, fileContents.remember(workdir, after))
	for i := range changes {
		if d, ok := diffs[changes[i].path]; ok {
			changes[i].hasDiff, changes[i].plus, changes[i].minus = true, d.added, d.removed
		}
	}
	msg := formatFileVersions(name, changes)
	path, err := writeFileDiffs(logDir, agentID, time.Now(), changes, diffs)
	if path != "" {
		msg += fmt.Sprintf("\n差分: `%s`", path)
	}
	return msg, err
}
