package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnifiedDiff(t *testing.T) {
	a := splitLines([]byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"))
	b := splitLines([]byte("1\n2\n3\n4\nfive\n6\n7\n8\n9\n10\n11\n12\nadd\n"))
	d := unifiedDiff("x.go", a, b)
	want := "--- a/x.go\n+++ b/x.go\n" +
		"@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n" +
		"@@ -10,3 +10,4 @@\n 10\n 11\n 12\n+add\n"
	if d.unified != want {
		t.Fatalf("差分:\n%s\nwant:\n%s", d.unified, want)
	}
	if d.added != 2 || d.removed != 1 {
		t.Errorf("行数 +%d -%d", d.added, d.removed)
	}
	// 新規ファイル（空から）と、末尾に改行がない行
	d = unifiedDiff("n.txt", nil, splitLines([]byte("a\nb")))
	if !strings.Contains(d.unified, "@@ -0,0 +1,2 @@\n+a\n+b\n\\ No newline at end of file\n") {
		t.Errorf("新規:\n%s", d.unified)
	}
	// CRLF の違いだけなら差分なし
	if d := unifiedDiff("c.txt", splitLines([]byte("a\r\nb\r\n")), splitLines([]byte("a\nb\n"))); d.unified != "" {
		t.Errorf("改行コードだけの違いで差分が出た:\n%s", d.unified)
	}
}

// ターンの前後で、変わったファイルの行数と差分ファイルのパスがメッセージに入り、差分ファイルに中身が書かれる
func TestFileVersionsMessage(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("edit.go", "a\nb\nc\n")
	write("gone.go", "x\n")
	write("bin.dat", "\x00\x01")
	cache := &fileContentCache{}
	saved := fileContents
	fileContents = cache
	defer func() { fileContents = saved }()

	before := takeFileSnapshot(dir, logDir)
	beforeData := fileContents.remember(dir, before)
	if _, ok := beforeData["bin.dat"]; ok {
		t.Fatal("バイナリを控えている")
	}

	write("edit.go", "a\nB\nc\n")
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(filepath.Join(dir, "edit.go"), future, future)
	write("new.go", "hello\n")
	write("bin.dat", "\x00\x02")
	os.Chtimes(filepath.Join(dir, "bin.dat"), future, future)
	os.Remove(filepath.Join(dir, "gone.go"))

	msg, err := fileVersionsMessage("Claude Code 2", "claude2", dir, logDir, before, beforeData)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`edit.go` 更新", "+1 -1", "`gone.go` 削除 +0 -1", "`new.go` 新規", "差分: `"} {
		if !strings.Contains(msg, want) {
			t.Errorf("%q を含まない:\n%s", want, msg)
		}
	}
	if line := strings.SplitN(strings.SplitAfter(msg, "`bin.dat`")[1], "\n", 2)[0]; strings.Contains(line, "+") {
		t.Errorf("バイナリに行数が付いた:\n%s", msg)
	}
	i := strings.Index(msg, "差分: `")
	path := strings.TrimSuffix(msg[i+len("差分: `"):], "`")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, filepath.Join(logDir, "diffs")) || !strings.Contains(string(data), "-b\n+B\n") || !strings.Contains(string(data), "+++ b/new.go\n@@ -0,0 +1,1 @@\n+hello\n") {
		t.Errorf("差分ファイル %s:\n%s", path, data)
	}

	// 変更がなければ何も出さず、差分ファイルも作らない
	snap := takeFileSnapshot(dir, logDir)
	if msg, err := fileVersionsMessage("x", "x", dir, logDir, snap, fileContents.remember(dir, snap)); msg != "" || err != nil {
		t.Errorf("変更なし: %q %v", msg, err)
	}
}

func TestSecretFileNotDiffed(t *testing.T) {
	for name, want := range map[string]bool{
		"server.pem": true, "conf/Credentials.json": true, "id_rsa": true, ".secret": true, "api_token.txt": true,
		"main.go": false, "auth.go": false, "README.md": false,
	} {
		if got := isSecretFile(name); got != want {
			t.Errorf("isSecretFile(%q) = %v, want %v", name, got, want)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "credentials.json"), []byte("{\"key\":\"s3cr3t\"}\n"), 0o644)
	snap := takeFileSnapshot(dir)
	if data := fileContents.remember(dir, snap); data["credentials.json"] != nil {
		t.Errorf("秘密情報のファイルの中身を控えた")
	}
}

func TestPruneFileDiffs(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"diff-20260101-000000-a.diff", "diff-20260102-000000-a.diff", "diff-20260103-000000-a.diff", "other.txt"} {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	pruneFileDiffs(dir, 2)
	left, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(left) != 3 || fileExists(filepath.Join(dir, "diff-20260101-000000-a.diff")) {
		t.Errorf("残ったファイル: %v", left)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
