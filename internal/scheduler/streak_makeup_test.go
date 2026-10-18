package scheduler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// makeupCell 一条 heatmap 单元格（date→score）。
type makeupCell struct {
	date  string
	score int
}

// makeupStub 模拟补签链路三个端点：heatmap / streak / makeup-cards/use。
// today 由响应给出（不依赖宿主机时钟），窗口判定因此可确定性断言。
type makeupStub struct {
	today      string // heatmap 的 today.date，缺省 2026-10-11
	cells      []makeupCell
	balance    int      // makeup_cards.balance
	launch     string   // streak.launch_date，缺省 2026-06-17
	noLaunch   bool     // 响应里不带 launch_date
	madeUp     []string // streak.makeup_dates：已补签历史（不是待办清单）
	useMsg     string   // use 业务错误文案，缺省 "date is not broken"
	useErr     bool     // use 返回业务错误（上游拒绝）
	heatmapErr bool     // heatmap 500
	heatmapRaw string   // 非空时 heatmap 直接返回该原始载荷（结构异常用例）

	heatmapHits atomic.Int32
	streakHits  atomic.Int32

	mu   sync.Mutex
	used []string // 实际提交的 target_date，按提交顺序
}

func (s *makeupStub) usedDates() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.used))
	copy(out, s.used)
	return out
}

func (s *makeupStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/heatmap":
			s.heatmapHits.Add(1)
			if s.heatmapErr {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			if s.heatmapRaw != "" {
				w.Write([]byte(s.heatmapRaw))
				return
			}
			today := s.today
			if today == "" {
				today = "2026-10-11"
			}
			var b strings.Builder
			fmt.Fprintf(&b, `{"code":0,"data":{"today":{"date":"%s","score":2},"cells":[`, today)
			for i, c := range s.cells {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"date":"%s","score":%d}`, c.date, c.score)
			}
			b.WriteString(`]}}`)
			w.Write([]byte(b.String()))
		case "/activity/growth/streak":
			s.streakHits.Add(1)
			launch := s.launch
			if launch == "" {
				launch = "2026-06-17"
			}
			launchField := fmt.Sprintf(`"launch_date":"%s",`, launch)
			if s.noLaunch {
				launchField = ""
			}
			madeUp := `[]`
			if s.madeUp != nil {
				raw, _ := json.Marshal(s.madeUp)
				madeUp = string(raw)
			}
			fmt.Fprintf(w, `{"code":0,"data":{%s"streak":{"days":3,"makeup_dates":%s},`+
				`"makeup_cards":{"balance":%d,"max":4}}}`, launchField, madeUp, s.balance)
		case "/activity/growth/makeup-cards/use":
			var body struct {
				Target string `json:"target_date"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.used = append(s.used, body.Target)
			s.mu.Unlock()
			if s.useErr {
				msg := s.useMsg
				if msg == "" {
					msg = "date is not broken"
				}
				w.WriteHeader(400)
				fmt.Fprintf(w, `{"code":400,"msg":%q}`, msg)
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// makeupForTest 走真实调用路径：先查连登，再判断/执行补签（补签成功时内部会重查连登）。
func makeupForTest(t *testing.T, s *Scheduler, a *auth.Auth) {
	t.Helper()
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		t.Fatalf("GrowthStreakFull: %v", err)
	}
	if _, err := s.makeupMissed(a, full); err != nil {
		t.Errorf("makeupMissed: %v", err)
	}
}

// makeupScheduler 构造只含一个账号的调度器，指向 stub。
func makeupScheduler(t *testing.T, stub *makeupStub) (*Scheduler, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), a
}

// 本月三种格子 + 两个边界：上月活跃（不许补）、今天 0 分（未结束，不许补）。
func makeupCells() []makeupCell {
	return []makeupCell{
		{"2026-09-29", 5},
		{"2026-10-01", 2}, {"2026-10-02", 2}, {"2026-10-03", 2}, {"2026-10-04", 2},
		{"2026-10-05", 0}, {"2026-10-06", 0}, {"2026-10-07", 0},
		{"2026-10-08", 2}, {"2026-10-09", 2}, {"2026-10-10", 2},
		{"2026-10-11", 0},
	}
}

// TestMakeupMissedFillsNearestFirst 三处漏签、两张卡：只补最近两天，且从近到远。
func TestMakeupMissedFillsNearestFirst(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 2}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	got := stub.usedDates()
	want := []string{"2026-10-07", "2026-10-06"}
	if len(got) != len(want) {
		t.Fatalf("补签提交=%v want %v（卡数封顶 + 只补本月漏签）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("补签提交=%v want %v（最近的优先）", got, want)
		}
	}
}

// TestMakeupMissedNoBrokenNoWrite 本月无漏签：一个写请求都不发。
func TestMakeupMissedNoBrokenNoWrite(t *testing.T) {
	cells := makeupCells()
	for i := range cells {
		if cells[i].score == 0 {
			cells[i].score = 2
		}
	}
	stub := &makeupStub{cells: cells, balance: 2}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（无漏签）", got)
	}
}

// TestMakeupMissedNoCardNoWrite 有漏签但无补签卡：不发写请求。
func TestMakeupMissedNoCardNoWrite(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 0}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（无卡）", got)
	}
}

// TestMakeupMissedStopsOnReject 上游拒绝（date is not broken）：立即停，不继续往前补。
// 注册前的日子同样是 0 分，无法与漏签区分，靠拒绝兜底——所以拒绝必须终止本轮。
func TestMakeupMissedStopsOnReject(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 3, useErr: true}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	got := stub.usedDates()
	if len(got) != 1 || got[0] != "2026-10-07" {
		t.Errorf("补签提交=%v want [2026-10-07]（拒绝即停，最新一天先试）", got)
	}
}

// TestMakeupMissedHeatmapErrorSilent heatmap 查询失败：静默返回，不发写请求。
func TestMakeupMissedHeatmapErrorSilent(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 2, heatmapErr: true}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a) // 不应 panic

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（查询失败不猜）", got)
	}
}

// TestMakeupMissedRepeatedRunStopsAfterCardsUsed 幂等性护栏：卡用光后下一轮不再发写请求。
func TestMakeupMissedRepeatedRunStopsAfterCardsUsed(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 1}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)
	stub.balance = 0 // 上游已扣卡
	makeupForTest(t, s, a)

	got := stub.usedDates()
	if len(got) != 1 || got[0] != "2026-10-07" {
		t.Errorf("两轮补签提交=%v want [2026-10-07]（第二轮无卡不发写）", got)
	}
}

// captureSchedLog 捕获 log 输出（log.SetOutput 是进程级全局，本组测试不并行）。
func captureSchedLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestMakeupMissedSkipsAlreadyMadeUpDates 回归（上游 245f3138）：streak.makeup_dates
// 是**已补登历史**，不是待办清单。旧实现把它当待补日期，会每轮重复提交已补过的日子；
// 真正待补的日期只能来自活跃日历的 score == 0。
func TestMakeupMissedSkipsAlreadyMadeUpDates(t *testing.T) {
	stub := &makeupStub{cells: makeupCells(), balance: 2, madeUp: []string{"2026-10-07"}}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	got := stub.usedDates()
	want := []string{"2026-10-06", "2026-10-05"}
	if len(got) != len(want) {
		t.Fatalf("补签提交=%v want %v（已补登日期必须排除）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("补签提交=%v want %v（跳过已补登、最近的优先）", got, want)
		}
	}
}

// TestMakeupMissedHistoryOnlyNeverResubmits 只有补签历史、没有新断登：一个写请求都不发，
// 连跑两轮也不会把历史日期再提交一次（旧实现在这里每轮都会补 10-07）。
func TestMakeupMissedHistoryOnlyNeverResubmits(t *testing.T) {
	cells := makeupCells()
	history := []string{"2026-10-05", "2026-10-06", "2026-10-07"}
	for i := range cells {
		if cells[i].score == 0 {
			cells[i].score = 2
		}
	}
	stub := &makeupStub{cells: cells, balance: 2, madeUp: history}
	s, a := makeupScheduler(t, stub)

	for i := 0; i < 2; i++ {
		makeupForTest(t, s, a)
	}

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（历史不是待办）", got)
	}
}

// TestMakeupMissedMissingLaunchDateStopsAndWarns 上线日期缺失：不能猜补登窗口，
// 本轮不补，且必须留 WARN（失败提醒不能被其他步骤的成功盖掉）。
func TestMakeupMissedMissingLaunchDateStopsAndWarns(t *testing.T) {
	buf := captureSchedLog(t)
	stub := &makeupStub{cells: makeupCells(), balance: 2, noLaunch: true}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（窗口不可判定不花卡）", got)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("日志=%q want 含 WARN（缺 launch_date 必须提醒）", buf.String())
	}
}

// TestMakeupMissedInvalidCalendarStopsAndWarns 日历结构异常（分数是字符串"0"）：
// 不能当成「0 分断登」，本轮停手并提醒。
func TestMakeupMissedInvalidCalendarStopsAndWarns(t *testing.T) {
	buf := captureSchedLog(t)
	stub := &makeupStub{
		balance:    2,
		heatmapRaw: `{"code":0,"data":{"today":{"date":"2026-10-11"},"cells":[{"date":"2026-10-07","score":"0"}]}}`,
	}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 0 {
		t.Errorf("补签提交=%v want 空（异常日历不花卡）", got)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("日志=%q want 含 WARN（日历异常必须提醒）", buf.String())
	}
}

// TestMakeupMissedBenignRaceDoesNotWarn 查询后日期已被补上（400 + 明确文案）：
// 属正常竞争，不记失败、不打 WARN，且不继续往前补。
func TestMakeupMissedBenignRaceDoesNotWarn(t *testing.T) {
	buf := captureSchedLog(t)
	stub := &makeupStub{cells: makeupCells(), balance: 3, useErr: true,
		useMsg: "date is not broken, no makeup needed"}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 1 || got[0] != "2026-10-07" {
		t.Errorf("补签提交=%v want [2026-10-07]（竞争即停）", got)
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("日志=%q want 无 WARN（良性竞争不算失败）", buf.String())
	}
}

// TestMakeupMissedUnknownRejectWarns 上游未知拒绝：必须留 WARN。
func TestMakeupMissedUnknownRejectWarns(t *testing.T) {
	buf := captureSchedLog(t)
	stub := &makeupStub{cells: makeupCells(), balance: 3, useErr: true, useMsg: "invalid request"}
	s, a := makeupScheduler(t, stub)

	makeupForTest(t, s, a)

	if got := stub.usedDates(); len(got) != 1 {
		t.Errorf("补签提交=%v want 1 次（拒绝即停）", got)
	}
	if !strings.Contains(buf.String(), "WARN") {
		t.Errorf("日志=%q want 含 WARN（未知拒绝必须提醒）", buf.String())
	}
}
