package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// compStub 记录补偿两端点的调用次数，并按 statusCode/statusBody 回固定形态。
// 其余路径一律 404（成长/连登链路在此处自然失败早退，不影响补偿断言）。
type compStub struct {
	mu          sync.Mutex
	paths       map[string]int
	statusCode  int
	statusBody  string
	claimBody   string
	claimCalls  int
	statusCalls int
}

func (c *compStub) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		if c.paths == nil {
			c.paths = map[string]int{}
		}
		c.paths[r.URL.Path]++
		c.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/compensation-status"):
			c.mu.Lock()
			c.statusCalls++
			code, body := c.statusCode, c.statusBody
			c.mu.Unlock()
			if code != 0 {
				http.Error(w, body, code)
				return
			}
			_, _ = w.Write([]byte(body))
		case strings.HasSuffix(r.URL.Path, "/claim-compensation"):
			c.mu.Lock()
			c.claimCalls++
			body := c.claimBody
			c.mu.Unlock()
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
}

func (c *compStub) counts() (status, claim int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusCalls, c.claimCalls
}

// compScheduler 构造只含一个 CN 个人号的调度器（补偿链路的最小上下文）。
func compScheduler(t *testing.T, stub *compStub) *Scheduler {
	t.Helper()
	srv := stub.server()
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

// TestStreakBonusSkipsClaimWhenNotPending 无待领时**不得**发起领取——这是本改动的核心：
// 旧写法是无条件盲领，于是「无补偿 / 已领过」也打了假的「🎊 补偿领取 +0c」成功行
// （线上一个月 20 行全是 +0c）。本用例在旧代码上必红（盲领会命中 /claim-compensation）。
func TestStreakBonusSkipsClaimWhenNotPending(t *testing.T) {
	stub := &compStub{statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":false,"credit_num":0}}`}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 {
		t.Errorf("状态查询次数=%d want 1（每号每天一次）", status)
	}
	if claim != 0 {
		t.Errorf("无待领却发起了 %d 次领取（盲领回归）", claim)
	}
}

// TestStreakBonusClaimsWhenPending 待领时恰好领一次。
func TestStreakBonusClaimsWhenPending(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500,"end_time":"2026-10-22 23:59:59"}}`,
		claimBody:  `{"code":0,"msg":"OK","data":{"credit":500}}`,
	}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 || claim != 1 {
		t.Errorf("status=%d claim=%d want 1/1", status, claim)
	}
}

// TestStreakBonusStatusFailureDoesNotClaim 状态查询失败（5xx）时不领取、不重试，
// 且不得影响后续链路（本函数返回后 streakBonusAccount 继续跑连登/兑换）。
func TestStreakBonusStatusFailureDoesNotClaim(t *testing.T) {
	stub := &compStub{statusCode: http.StatusInternalServerError, statusBody: `{"code":500,"msg":"boom"}`}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	status, claim := stub.counts()
	if status != 1 {
		t.Errorf("状态查询次数=%d want 1（失败不重试）", status)
	}
	if claim != 0 {
		t.Errorf("状态未知时不该领取，实际 %d 次", claim)
	}
}

// TestStreakBonusClaimFailureNotRetried 领取失败只发一次（不重试、不掩盖）。
func TestStreakBonusClaimFailureNotRetried(t *testing.T) {
	stub := &compStub{
		statusBody: `{"code":0,"msg":"OK","data":{"claimed":false,"active":true,"credit_num":500}}`,
		claimBody:  `{"code":500,"msg":"boom"}`,
	}
	s := compScheduler(t, stub)
	s.RunStreakBonusNow()

	if _, claim := stub.counts(); claim != 1 {
		t.Errorf("领取次数=%d want 1（失败不重试）", claim)
	}
}
