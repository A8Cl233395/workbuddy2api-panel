package upstream

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// DumpDir 为空时零改动：同一个 reader 原样返回，不建目录不写文件。
func TestDumpUpstreamDisabled(t *testing.T) {
	c := New()
	rc := io.NopCloser(strings.NewReader("data: x\n\n"))
	if got := c.dumpUpstream(rc, []byte(`{}`), nil); got != io.ReadCloser(rc) {
		t.Fatalf("DumpDir 为空时应原样返回 reader")
	}
}

// 打开后一次请求一个自包含文件：出站请求体 + 上游原始字节，且字节不被改写。
func TestDumpUpstreamWritesRaw(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.DumpDir = dir
	raw := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\ndata: [DONE]\n\n"
	a := &auth.Auth{UID: "0011223344556677", Domain: "www.codebuddy.cn"}

	rc := c.dumpUpstream(io.NopCloser(strings.NewReader(raw)), []byte(`{"model":"m"}`), a)
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != raw {
		t.Fatalf("透传字节被改写: %q err=%v", got, err)
	}
	rc.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(files) != 1 {
		t.Fatalf("期望 1 个 dump 文件，得到 %d", len(files))
	}
	b, _ := os.ReadFile(files[0])
	s := string(b)
	if !strings.Contains(s, `{"model":"m"}`) || !strings.HasSuffix(s, raw) {
		t.Fatalf("dump 文件应含请求体与原始响应，实际 %q", s)
	}
	if strings.Contains(filepath.Base(files[0]), "0011223344556677") {
		t.Fatalf("文件名不应含完整 UID: %s", files[0])
	}
}
