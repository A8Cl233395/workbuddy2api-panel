//go:build ignore

// probe169 是 issue #169 的差分实验探针（不进构建，就地 go run 使用）。
//
// 目的：验证「把派生会话键回写进 body.conversation_id」是否
//   A) 被上游渠道校验接受（不触发 11-128 / 11140 之类的 body 形态拒绝）；
//   B) 真能提高 prompt cache 命中（上游按 body.conversation_id 分缓存槽）。
//
// 用法（在仓库根目录）：
//
//	go run internal/probe/probe169.go -auths /e/dev/tabbit2api-panel/auths -realm cn
//
// 只打印状态码 / 错误码 / usage 里的缓存字段，不打印任何 token。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	authDir := flag.String("auths", "", "auth 文件目录")
	realm := flag.String("realm", "cn", "cn / global")
	model := flag.String("model", "deepseek-v4.1-flash", "模型 id（裸名）")
	tokFile := flag.String("tokfile", "", "token 文件（首行为 access token，不回显）")
	uid := flag.String("uid", "probe169uid", "账号 uid（prompt_cache_key 隔离段用）")
	base := flag.String("base", "", "覆盖 chat base")
	flag.Parse()

	var a *auth.Auth
	if *tokFile != "" {
		raw, err := os.ReadFile(*tokFile)
		if err != nil {
			fmt.Println("read tokfile:", err)
			os.Exit(1)
		}
		tok := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		a = &auth.Auth{AccessToken: tok, UID: *uid, ExpiresAt: time.Now().Add(time.Hour).Unix()}
		_, _ = auth.BackfillRealmFor(a, *realm)
		fmt.Printf("token loaded: len=%d realm=%s\n", len(tok), a.Realm())
	} else {
		a = loadAuth(*authDir, *realm)
	}
	if a == nil {
		fmt.Printf("FAIL: 没有可用的 realm=%s 账号\n", *realm)
		os.Exit(1)
	}
	uidShow := a.UID
	if len(uidShow) > 8 {
		uidShow = uidShow[:8]
	}
	fmt.Printf("account ready: uid=%s realm=%s\n", uidShow, a.Realm())

	c := upstream.New()
	if *base != "" {
		if a.IsGlobal() {
			c.ChatBaseGlobal = *base
		} else {
			c.ChatBaseCN = *base
		}
	}
	fmt.Printf("chat base: cn=%s global=%s\n", c.ChatBaseCN, c.ChatBaseGlobal)

	fmt.Println("\n=== 实验 A：短请求，带 / 不带 conversation_id ===")
	shortMsgs := `[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"回复一个字：好"}]`
	runCase(c, a, *model, "A1 带 conversation_id", shortMsgs, "probe169-conv-fixed")
	time.Sleep(1200 * time.Millisecond)
	runCase(c, a, *model, "A2 不带 conversation_id", shortMsgs, "")
	time.Sleep(1200 * time.Millisecond)
	runCase(c, a, *model, "A3 带随机 different id", shortMsgs, "probe169-"+time.Now().Format("150405"))

	fmt.Println("\n=== 实验 B：长前缀（约 3k tokens），测缓存命中 ===")
	longMsg := buildLongMessages()
	// B1：同一 conversation_id 连发两次（期望第二次命中）
	runCase(c, a, *model, "B1-a 固定 conv（第1次）", longMsg, "probe169-cache-fixed")
	time.Sleep(1500 * time.Millisecond)
	runCase(c, a, *model, "B1-b 固定 conv（第2次）", longMsg, "probe169-cache-fixed")
	time.Sleep(1500 * time.Millisecond)
	// B2：不带 conversation_id 连发两次（现状：每次可能开新槽）
	runCase(c, a, *model, "B2-a 无 conv（第1次）", longMsg, "")
	time.Sleep(1500 * time.Millisecond)
	runCase(c, a, *model, "B2-b 无 conv（第2次）", longMsg, "")
}

// parseLoose 兼容两种 auth 文件格式：
//   - 本仓标准：accessToken / uid / realm / expiresAt
//   - 其它面板导出：token / user_id / realm / expires_at（毫秒）
func parseLoose(raw []byte) *auth.Auth {
	a, err := auth.Parse(raw)
	if err == nil && a != nil && a.AccessToken != "" {
		return a
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	tok := str("accessToken", "token", "access_token")
	if tok == "" {
		return nil
	}
	exp := int64(0)
	for _, k := range []string{"expiresAt", "expires_at"} {
		if v, ok := m[k].(float64); ok && v > 0 {
			exp = int64(v)
			break
		}
	}
	if exp > 1e12 { // 毫秒 → 秒
		exp /= 1000
	}
	a = &auth.Auth{
		AccessToken: tok,
		UID:         str("uid", "user_id", "userId"),
		ExpiresAt:   exp,
		Nickname:    str("nickname"),
	}
	// realm 归一：国际版导出常用 intl。
	switch str("realm") {
	case "intl", "global", "international":
		_, _ = auth.BackfillRealmFor(a, "global")
	default:
		_, _ = auth.BackfillRealmFor(a, "cn")
	}
	return a
}

// loadAuth 从目录里挑一个指定 realm 的可用账号。
func loadAuth(dir, realm string) *auth.Auth {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("read auths:", err)
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		a := parseLoose(raw)
		if a == nil {
			continue
		}
		// 扁平形 auth 文件的 realm 是 "cn"/"intl" 等自由值：只认我们要的域。
		got := a.Realm()
		if realm == "global" {
			if got != "global" && got != "intl" {
				continue
			}
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				continue
			}
		} else {
			if got != "cn" {
				continue
			}
			if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
				continue
			}
		}
		if a.ExpiresAt > 0 && time.Now().Unix() > a.ExpiresAt {
			continue // 过期不测
		}
		return a
	}
	return nil
}

// buildLongMessages 造一个约 3k token 的稳定前缀（同一会话重复请求时逐字节相同）。
func buildLongMessages() string {
	sentence := "缓存分区实验素材：gateway observes upstream prompt cache behaviour by replaying an identical long prefix. "
	var b strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&b, "%03d %s\n", i, strings.Repeat(sentence, 1))
	}
	payload := strings.TrimSpace(b.String())
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": payload + "\n\n只回复一个字：好"},
		},
	}
	out, _ := json.Marshal(body["messages"])
	return string(out)
}

// runCase 发一次真实 chat 请求并打印状态与 usage 缓存字段。
func runCase(c *upstream.Client, a *auth.Auth, model, label, messagesJSON, convID string) {
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":%s}`, model, messagesJSON)
	if convID != "" {
		// 顶层注入：模拟网关回写 conversation_id（本实验唯一变量）。
		var obj map[string]any
		if json.Unmarshal([]byte(body), &obj) != nil {
			fmt.Printf("%-26s BODY-INVALID\n", label)
			return
		}
		obj["conversation_id"] = convID
		nb, _ := json.Marshal(obj)
		body = string(nb)
	}
	meta := upstream.ChatMeta{ConversationRequestID: "probe169-" + time.Now().Format("150405.000")}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	start := time.Now()
	rc, status, respBody, err := c.ChatStreamContext(ctx, a, []byte(body), "", meta, nil)
	ttfb := time.Since(start)
	if err != nil {
		fmt.Printf("%-26s ERR status=%d err=%v body=%s\n", label, status, err, truncate(string(respBody), 220))
		return
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	usage := extractUsage(string(raw))
	fmt.Printf("%-26s status=%d ttfb=%5dms %s\n", label, status, ttfb.Milliseconds(), usage)
	if status != 200 {
		fmt.Printf("%-26s raw=%s\n", "", truncate(string(raw), 260))
	}
}

// extractUsage 从 SSE 里取末帧 usage 的缓存字段。
func extractUsage(s string) string {
	var prompt, hit, miss, cached, total int64
	seen := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		u, ok := obj["usage"].(map[string]any)
		if !ok {
			continue
		}
		seen = true
		prompt = num(u["prompt_tokens"])
		total = num(u["total_tokens"])
		if v, ok := u["prompt_cache_hit_tokens"]; ok {
			hit = num(v)
		}
		if v, ok := u["prompt_cache_miss_tokens"]; ok {
			miss = num(v)
		}
		if v, ok := u["cached_tokens"]; ok {
			cached = num(v)
		}
		if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
			cached = num(d["cached_tokens"])
		}
	}
	if !seen {
		return "usage=<none>"
	}
	rate := 0.0
	if prompt > 0 {
		rate = float64(hit) * 100 / float64(prompt)
	}
	return fmt.Sprintf("prompt=%d total=%d hit=%d miss=%d cached=%d hit%%=%.1f", prompt, total, hit, miss, cached, rate)
}

func num(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
