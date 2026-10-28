package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/prompt"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// captureChatUpstream 记录每次出站 chat 请求体；behavior 决定第 n 次（从 0 起）
// 返回的状态码与响应体，便于断言重试次数。
func captureChatUpstream(bodies *[][]byte, behavior func(call int) (status int, body string)) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, raw)
			status, body := behavior(len(*bodies) - 1)
			ct := "application/json"
			if status == http.StatusOK {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
}

// outboundSystemContents 提取出站 body 里所有 system 消息的 content。
// 只处理纯文本 content（本文件的 fixture 形态）；多模态 parts 不走这里。
func outboundSystemContents(t *testing.T, body []byte) []string {
	t.Helper()
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("outbound body json: %v (body=%s)", err, body)
	}
	var out []string
	for _, m := range obj.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		s, ok := m.Content.(string)
		if !ok {
			t.Fatalf("system content is not string: %T", m.Content)
		}
		out = append(out, s)
	}
	return out
}

// TestPromptDegradeSwitchMatrix 覆盖矩阵：
// {passthrough, append} × {degrade_enabled=true, false} × {未降级, 降级窗口激活}。
// 断言出站 system 内容符合开关语义；窗口激活时只有开启才切换到中性提示词。
func TestPromptDegradeSwitchMatrix(t *testing.T) {
	const (
		clientSys  = "CLIENT_SYSTEM_FINGERPRINT" // 无句末标点：降级时走「搬家」而非拆句
		gatewaySys = "GATEWAY_SYSTEM_PROMPT"
	)
	cases := []struct {
		name   string
		mode   string
		enable bool
		active bool
		want   []string
	}{
		{"passthrough/enabled/inactive", "passthrough", true, false, []string{clientSys}},
		{"passthrough/disabled/inactive", "passthrough", false, false, []string{clientSys}},
		{"passthrough/enabled/active", "passthrough", true, true, []string{prompt.Degraded}},
		{"passthrough/disabled/active", "passthrough", false, true, []string{clientSys}},
		// append 的正常语义是「客户端 system 原样 + 网关 system 追加」；关闭降级
		// 只关掉中性提示词替换，不改变 append 本身。
		{"append/enabled/inactive", "append", true, false, []string{clientSys, gatewaySys}},
		{"append/disabled/inactive", "append", false, false, []string{clientSys, gatewaySys}},
		{"append/enabled/active", "append", true, true, []string{prompt.Degraded}},
		{"append/disabled/active", "append", false, true, []string{clientSys, gatewaySys}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var bodies [][]byte
			up := captureChatUpstream(&bodies, func(int) (int, string) { return 200, sseOK })
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{
				Pool:                 p,
				Upstream:             up,
				PromptMode:           tc.mode,
				PromptText:           gatewaySys,
				DisablePromptDegrade: !tc.enable,
			})
			if tc.active {
				h.degrade.Trigger()
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(fmt.Sprintf(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":%q},{"role":"user","content":"hi"}]}`, clientSys))))
			if rec.Code != 200 {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if len(bodies) != 1 {
				t.Fatalf("want 1 upstream call, got %d", len(bodies))
			}
			if got := outboundSystemContents(t, bodies[0]); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("outbound system = %q, want %q; body=%s", got, tc.want, bodies[0])
			}
			// 可观测标记只应出现在真正降级生效的请求上。
			wantHeader := ""
			if tc.enable && tc.active {
				wantHeader = "1"
			}
			if got := rec.Header().Get("X-WB2A-Degraded"); got != wantHeader {
				t.Errorf("X-WB2A-Degraded=%q want %q", got, wantHeader)
			}
		})
	}
}

// TestPromptDegradeDisabledPassesBlockedErrorWithoutRetry 关闭开关后：
//   - 被拦截请求只打一次上游，不再走拆句/搬家/中性提示词三层重试；
//   - 客户端收到上游原始错误（error-passthrough）；
//   - 不触发降级窗口；已在内存激活的窗口也继续透传原始 system；
//   - ErrContentBlocked 仍然不罚账号（无冷却/熔断/计错）。
func TestPromptDegradeDisabledPassesBlockedErrorWithoutRetry(t *testing.T) {
	const upstreamErr = `{"code":11-128,"msg":"blocked by security policy"}`
	for _, mode := range []string{"passthrough", "append"} {
		for _, preActive := range []bool{false, true} {
			name := mode + "/window-inactive"
			if preActive {
				name = mode + "/window-active"
			}
			t.Run(name, func(t *testing.T) {
				var bodies [][]byte
				up := captureChatUpstream(&bodies, func(int) (int, string) { return 400, upstreamErr })
				p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
				h := NewHandler(Config{
					Pool:                 p,
					Upstream:             up,
					PromptMode:           mode,
					PromptText:           "GATEWAY_SYSTEM_PROMPT",
					DisablePromptDegrade: true,
				})
				if preActive {
					h.degrade.Trigger()
				}

				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT_SYSTEM_FINGERPRINT"},{"role":"user","content":"hi"}]}`)))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("code=%d want 400; body=%s", rec.Code, rec.Body)
				}
				if len(bodies) != 1 {
					t.Fatalf("degrade disabled must not retry, got %d upstream calls", len(bodies))
				}
				if !strings.Contains(rec.Body.String(), "blocked by security policy") {
					t.Errorf("client should receive upstream original error: %s", rec.Body)
				}
				if got := outboundSystemContents(t, bodies[0]); len(got) == 0 || got[0] != "CLIENT_SYSTEM_FINGERPRINT" {
					t.Errorf("client system should stay unchanged, got %q", got)
				}
				if strings.Contains(string(bodies[0]), prompt.Degraded) {
					t.Errorf("disabled request must not use degraded prompt: %s", bodies[0])
				}
				if !preActive && h.degrade.Active() {
					t.Error("degrade window must not be triggered when switch is off")
				}
				if got := rec.Header().Get("X-WB2A-Degraded"); got != "" {
					t.Errorf("X-WB2A-Degraded=%q want empty", got)
				}
				if st, _ := p.Status("u1"); st.Cooling || st.Disabled || st.ErrTotal != 0 {
					t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
				}
			})
		}
	}
}

// TestPromptDegradeEnabledRetriesAndTriggersWindow 开启时保持既有语义：
// 第一次 400 → 中性提示词重试 → 200，并触发窗口、带可观测响应头。
func TestPromptDegradeEnabledRetriesAndTriggersWindow(t *testing.T) {
	var bodies [][]byte
	up := captureChatUpstream(&bodies, func(call int) (int, string) {
		if call == 0 {
			return 400, `{"code":11-128,"msg":"blocked by security policy"}`
		}
		return 200, sseOK
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls, got %d", len(bodies))
	}
	if got := outboundSystemContents(t, bodies[1]); len(got) != 1 || got[0] != prompt.Degraded {
		t.Errorf("retry system = %q, want degraded prompt", got)
	}
	if !h.degrade.Active() {
		t.Error("successful degraded retry should trigger the window")
	}
	if got := rec.Header().Get("X-WB2A-Degraded"); got != "1" {
		t.Errorf("X-WB2A-Degraded=%q want 1", got)
	}
}

// TestPromptDegradeCustomModeUnaffected custom 模式不进降级路径：无论开关开闭、
// 窗口是否激活，出站 system 始终是网关自有提示词，被拦时只打一次上游。
func TestPromptDegradeCustomModeUnaffected(t *testing.T) {
	const customSys = "CUSTOM_GATEWAY_SYSTEM"
	for _, enable := range []bool{true, false} {
		for _, active := range []bool{false, true} {
			name := fmt.Sprintf("enabled=%v/active=%v", enable, active)
			t.Run(name, func(t *testing.T) {
				var bodies [][]byte
				up := captureChatUpstream(&bodies, func(int) (int, string) {
					return 400, `{"code":11-128,"msg":"blocked by security policy"}`
				})
				p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
				h := NewHandler(Config{
					Pool:                 p,
					Upstream:             up,
					PromptMode:           "custom",
					PromptText:           customSys,
					DisablePromptDegrade: !enable,
				})
				if active {
					h.degrade.Trigger()
				}

				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
					strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("code=%d want 400; body=%s", rec.Code, rec.Body)
				}
				if len(bodies) != 1 {
					t.Fatalf("custom mode must not degrade-retry, got %d upstream calls", len(bodies))
				}
				if got := outboundSystemContents(t, bodies[0]); len(got) != 1 || got[0] != customSys {
					t.Errorf("custom outbound system = %q, want %q", got, customSys)
				}
			})
		}
	}
}

// TestPromptDegradeCustomPromptText 自定义降级提示词（prompt.degraded_file 解析结果）
// 在降级路径中替换内置中性句。
func TestPromptDegradeCustomPromptText(t *testing.T) {
	const customDegraded = "CUSTOM_DEGRADED_PROMPT"
	var bodies [][]byte
	up := captureChatUpstream(&bodies, func(int) (int, string) { return 200, sseOK })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:               p,
		Upstream:           up,
		PromptMode:         "passthrough",
		PromptDegradedText: customDegraded,
	})
	h.degrade.Trigger()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"CLIENT_SYSTEM_FINGERPRINT"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := outboundSystemContents(t, bodies[0]); len(got) != 1 || got[0] != customDegraded {
		t.Errorf("outbound system = %q, want %q", got, customDegraded)
	}
}
