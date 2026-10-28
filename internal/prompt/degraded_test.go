package prompt

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDegraded 空路径 = 内置中性提示词；路径非空 = 读文件；不可读 fail fast。
func TestLoadDegraded(t *testing.T) {
	got, err := LoadDegraded("")
	if err != nil {
		t.Fatal(err)
	}
	if got != Degraded {
		t.Errorf("empty path should fall back to built-in Degraded, got %q", got)
	}

	dir := t.TempDir()
	pf := filepath.Join(dir, "degraded.md")
	const want = "自定义降级提示词"
	if err := os.WriteFile(pf, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadDegraded(pf)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("LoadDegraded=%q want %q", got, want)
	}

	if _, err := LoadDegraded(filepath.Join(dir, "missing.md")); err == nil {
		t.Fatal("missing degraded prompt file should fail fast")
	}
}
