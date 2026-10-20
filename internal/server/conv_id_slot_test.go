package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// slotFake 是「按 body 的 conversation_id 分前缀缓存槽」的假上游（issue #169 的机制模型）。
//
// 建模依据是上报人做过的单变量 2×2：同一段长前缀，只要 body.conversation_id 相同且
// 前缀逐字节相同 → 命中；不带该字段 / 换了值 → 上游开新槽 → 整段未命中。
// 这里只复刻这一条规则（不模拟限流、档位、计费等），用途是证明**网关侧的接线**真的
// 能把同一会话打进同一个槽，而不是证明上游的真实分槽策略（那只能真机测）。
type slotFake struct {
	mu     sync.Mutex
	slots  map[string]string // conversation_id → 该槽上次看到的前缀摘要
	reqs   []slotReq
	prefix int // 每次请求的 prompt_tokens（固定，便于断言）
}

type slotReq struct {
	HasConv string // 出站 body 里的 conversation_id（空 = 客户端/网关都没带）
	Slot    string // 本次实际落的槽（空 conversation_id → 每次新槽）
	Hit     int
	Miss    int
}

func newSlotFake(prefix int) *slotFake {
	return &slotFake{slots: map[string]string{}, prefix: prefix}
}

func (f *slotFake) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)

	conv, _ := obj["conversation_id"].(string)
	// 前缀摘要：只取 messages，保证「同一会话 + 同一前缀」才命中。
	msgsJSON, _ := json.Marshal(obj["messages"])
	sum := sha256.Sum256(msgsJSON)
	prefixDigest := hex.EncodeToString(sum[:8])

	f.mu.Lock()
	rec := slotReq{HasConv: conv}
	if conv == "" {
		// 没带会话字段 = 上游开新槽（每次都不命中）。
		rec.Slot = fmt.Sprintf("new-%d", len(f.reqs))
		rec.Miss = f.prefix
	} else {
		rec.Slot = conv
		if prev, ok := f.slots[conv]; ok && prev == prefixDigest {
			rec.Hit = f.prefix
		} else {
			f.slots[conv] = prefixDigest
			rec.Miss = f.prefix
		}
	}
	f.reqs = append(f.reqs, rec)
	f.mu.Unlock()

	body := fmt.Sprintf(
		"data: {\"id\":\"chatcmpl-slot\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"好\"}}]}\n\n"+
			"data: {\"id\":\"chatcmpl-slot\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":2,\"total_tokens\":%d,\"prompt_cache_hit_tokens\":%d,\"prompt_cache_miss_tokens\":%d}}\n\n"+
			"data: [DONE]\n\n",
		f.prefix, f.prefix+2, rec.Hit, rec.Miss)
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (f *slotFake) snapshot() []slotReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]slotReq, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// longBody 造一个 ≥1000 token 的稳定长前缀（同会话两次请求逐字节相同）。
func longBody() string {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "行%02d 这是一段用于前缀缓存实验的固定素材，逐字节相同才能命中。\n", i)
	}
	return fmt.Sprintf(`{"model":"glm-5.2","stream":false,"messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":%q}]}`, b.String())
}

// TestConversationIDWritebackHitsSameSlot 端到端机制验证（issue #169）：
//
//   - 开关关闭（现状）：同会话连续两次请求出站 body 都不带 conversation_id →
//     假上游判为「每次新槽」→ 两次都未命中；
//   - 开关打开：两次出站都带**同一个** conversation_id → 第二次命中整段前缀。
//
// 也就是说：这个开关确实能把同一会话的连续请求收敛到同一个前缀缓存槽。
// 上游真实的槽规则只能真机验（见 internal/probe/probe169.go）。
func TestConversationIDWritebackHitsSameSlot(t *testing.T) {
	run := func(writeBack bool) []slotReq {
		fake := newSlotFake(3000)
		up := &upstream.Client{
			HTTP:       &http.Client{Transport: fake},
			ChatBaseCN: "https://fake.example",
		}
		up.WriteConversationID.Store(writeBack)
		h := NewHandler(Config{
			Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
			Upstream: up,
		})
		for i := 0; i < 2; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(longBody())))
			if rec.Code != http.StatusOK {
				t.Fatalf("writeBack=%v 第 %d 次请求 status=%d body=%s", writeBack, i+1, rec.Code, rec.Body)
			}
		}
		return fake.snapshot()
	}

	off := run(false)
	if len(off) != 2 {
		t.Fatalf("开关关闭时应发出 2 次上游请求，got %d", len(off))
	}
	for i, r := range off {
		if r.HasConv != "" {
			t.Errorf("开关关闭时第 %d 次出站 body 不应带 conversation_id，got %q", i+1, r.HasConv)
		}
		if r.Hit != 0 {
			t.Errorf("开关关闭时第 %d 次不应命中，got hit=%d", i+1, r.Hit)
		}
	}

	on := run(true)
	if len(on) != 2 {
		t.Fatalf("开关打开时应发出 2 次上游请求，got %d", len(on))
	}
	if on[0].HasConv == "" || on[1].HasConv == "" {
		t.Fatal("开关打开时两次出站 body 都必须带 conversation_id")
	}
	if on[0].HasConv != on[1].HasConv {
		t.Errorf("同一会话两次请求的 conversation_id 必须相同：%q vs %q", on[0].HasConv, on[1].HasConv)
	}
	if on[0].Hit != 0 {
		t.Errorf("第一次（新槽）不应命中，got %d", on[0].Hit)
	}
	if on[1].Hit != 3000 {
		t.Errorf("第二次应整段命中（同槽同前缀），got hit=%d miss=%d", on[1].Hit, on[1].Miss)
	}
}

// TestConversationIDWritebackKeepsHeaderFamily 回写不得动会话头族：
// 头一直是齐的（官方客户端也只发头），body 字段只是增量，不能把既有头挤掉。
func TestConversationIDWritebackKeepsHeaderFamily(t *testing.T) {
	var got http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			got = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.WriteConversationID.Store(true)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(longBody())))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Root-Request-ID"} {
		if got.Get(k) == "" {
			t.Errorf("会话头族缺 %s（回写不得影响头族）", k)
		}
	}
}
