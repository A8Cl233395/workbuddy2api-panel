// compensation.go 补偿活动的执行侧（上游 PR #196 / issue #189）。
//
// 把签到排程里那次「盲领」改成「先查状态 → 待领才领」：旧写法
// `if credit, err := ClaimCompensation(...); err == nil { log }` 让三种情形同构——
// ① 没有补偿 ② 已经领过 ③ 真的失败（401/5xx/网络）；实测更糟：#1/#2 时上游返回
// code 0 且无 credit 字段，于是每天打一行**假的**「🎊 补偿领取 +0c」（线上一个月
// 20 行全是 +0c），而 global 号在 CN billing 端的 401 反倒零痕迹。补偿带截止日，
// 签到一停摆就静默过期。本文件的纪律：要么成功、要么显形——排程与面板共用这里的
// 同一份实现，两条路径的分支与判据不会漂移。
package scheduler

import (
	"log"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// CompensationRow 单账号补偿状态（面板 GET /panel/api/compensation 的行，
// 也用作逐号领取的结果：领取后 Credit/Claimed 为领取后事实）。
type CompensationRow struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	Realm      string `json:"realm,omitempty"`
	Active     bool   `json:"active"`
	Claimed    bool   `json:"claimed"`
	Credit     int64  `json:"credit"`             // 可领额度（领取成功后为实际到账）
	EndTime    string `json:"end_time,omitempty"` // 领取截止（上游原文，无时区）
	Skip       bool   `json:"skip,omitempty"`
	SkipReason string `json:"skip_reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

// compSkipReason 补偿门控（返回空串 = 可执行）：
//   - global：D4 无 CN 任务体系，且实测该端点在国际号上恒 401（旧代码每天盲领一次、静默）；
//   - 企业版：上游对 claim-compensation 一律 400「企业账号不支持该操作」（与 claim-gift 同批实测）；
//   - disabled：凭证可能已失效，发起无意义。
//
// paused **不跳过**：补偿是纯领奖 RPC，与「让位防风控」的选号语义无关（旅行/成长任务亦然）。
func compSkipReason(st pool.Status) string {
	switch {
	case st.Disabled:
		return "账号已禁用"
	case st.Realm == "global":
		return "国际区账号不适用国内区补偿"
	case st.Enterprise:
		return "企业版账号无个人补偿"
	}
	return ""
}

// CompensationRows 并发查全部账号的补偿状态（只读，不领取），**保池顺序**：
// 预分配等长切片、每个 goroutine 只写自己那个下标——完成顺序不影响行序（面板逐行
// 对齐靠它；按完成顺序 append 会让行序随机漂移）。并发 3 与 /panel/api/packages 同口径。
// 门控跳过的账号也出行（带 SkipReason），面板据此显形「为什么没查」。
func (s *Scheduler) CompensationRows() []CompensationRow {
	stats := s.cfg.Pool.List()
	out := make([]CompensationRow, len(stats))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range stats {
		wg.Add(1)
		go func(i int, st pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = s.compensationRowOf(st, false)
		}(i, st)
	}
	wg.Wait()
	return out
}

// CompensateOne 单账号「门控 → 查状态 → 待领才领」，返回结果行。
// 面板逐号按钮与全量按钮共用；排程侧经 claimCompensationQuiet 调用同一实现。
func (s *Scheduler) CompensateOne(uid string) CompensationRow {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CompensationRow{UID: uid, Skip: true, SkipReason: "账号不存在", Error: "account not found"}
	}
	return s.compensationRowOf(st, true)
}

// compensationRowOf 单账号处理：claim=false 只查状态，claim=true 待领则领。
func (s *Scheduler) compensationRowOf(st pool.Status, claim bool) CompensationRow {
	row := CompensationRow{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm}
	if reason := compSkipReason(st); reason != "" {
		row.Skip, row.SkipReason = true, reason
		return row
	}
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil {
		row.Skip, row.SkipReason, row.Error = true, "账号未加载", "account not loaded"
		return row
	}
	src, err := s.cfg.Upstream.GetCompensationStatus(a)
	if err != nil {
		row.Error = err.Error() // 查询失败必须显形（旧盲领正是在这里静默）
		return row
	}
	row.Active, row.Claimed, row.Credit, row.EndTime = src.Active, src.Claimed, src.CreditNum, src.EndTime
	if !claim || !src.Pending() {
		return row
	}
	credit, err := s.cfg.Upstream.ClaimCompensation(a)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Credit, row.Claimed = credit, true
	return row
}

// claimCompensationQuiet 排程侧单账号补偿：调同一份实现，只把结果落成日志。
// 无待领（无活动 / 已领 / 无额度）静默——这是正确形态，不是失败；查询失败与领取失败
// 一律 WARN（含可领额度与截止，便于判断是否还剩时间）。
func (s *Scheduler) claimCompensationQuiet(a *auth.Auth) int64 {
	row := s.CompensateOne(a.UID)
	label := logfmt.Label(a.UID, a.Nickname)
	switch {
	case row.Error != "" && !row.Claimed:
		log.Printf("WARN: streak-bonus %s: 补偿处理失败（可领 %dc，截止 %s）: %s", label, row.Credit, row.EndTime, row.Error)
	case row.Claimed && row.Credit > 0:
		log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc（截止 %s）", label, row.Credit, row.EndTime)
	}
	return row.Credit
}

// claimGiftQuiet 排程侧单账号新手礼包：与补偿同一纪律——先查状态，待领才领。
// 旧写法 `if credit, err := ClaimGift(a); err == nil { log }` 对「已领过」也返回 (0, nil)
// （上游 200 + 无 credit 字段），于是每个已领号每天打一行假的「🎊 新手礼包 +0c」。
// 待领才领：无活动/已领/无额度静默，查询与领取失败 WARN（含截止，礼包也带有效期）。
func (s *Scheduler) claimGiftQuiet(a *auth.Auth) int64 {
	label := logfmt.Label(a.UID, a.Nickname)
	src, err := s.cfg.Upstream.GetGiftStatus(a)
	if err != nil {
		// 已知没有个人礼包体系的账号（global / 企业 / 已禁用）查询失败属预期，静默——
		// 否则每天固定刷出几行噪声；CN 正常号的查询失败必须 WARN（旧盲领正是在这里无痕）。
		if st, ok := s.cfg.Pool.Status(a.UID); ok && (st.Realm == "global" || st.Enterprise || st.Disabled) {
			return 0
		}
		log.Printf("WARN: streak-bonus %s: 礼包状态查询失败: %v", label, err)
		return 0
	}
	if !src.Pending() {
		return 0
	}
	credit, err := s.cfg.Upstream.ClaimGift(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 礼包领取失败（可领 %dc，截止 %s）: %v", label, src.CreditNum, src.EndTime, err)
		return 0
	}
	log.Printf("streak-bonus %s: 🎊 新手礼包 +%dc（截止 %s）", label, credit, src.EndTime)
	return credit
}
