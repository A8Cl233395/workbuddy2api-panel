// compensation.go 补偿活动的执行侧（上游 PR #196 / issue #189）。
//
// 把签到排程里那次「盲领」改成「先查状态 → 待领才领」：旧写法
// `if credit, err := ClaimCompensation(...); err == nil { log }` 让三种情形同构——
// ① 没有补偿 ② 已经领过 ③ 真的失败（401/5xx/网络）；实测更糟：#1/#2 时上游返回
// code 0 且无 credit 字段，于是每天打一行**假的**「🎊 补偿领取 +0c」（线上一个月
// 20 行全是 +0c），而 global 号在 CN billing 端的 401 反倒零痕迹。补偿带截止日，
// 签到一停摆就静默过期。本文件的纪律：要么成功、要么显形。
package scheduler

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// claimCompensationQuiet 单账号「查状态 → 待领才领」：
//   - 无待领（无活动 / 已领 / 无额度）→ 静默（这是正确形态，不是失败）；
//   - 状态查询失败、领取失败 → 显式 WARN（含可领额度与截止，便于判断是否还剩时间）。
//
// 门控由调用方负责（streakBonusAccount 只在非 global 非企业号上跑，见 RunStreakBonusNow）：
// 面板侧另走 CompensateOne / CompensateAll，门控在其内部。
func (s *Scheduler) claimCompensationQuiet(a *auth.Auth) int64 {
	st, err := s.cfg.Upstream.GetCompensationStatus(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 补偿状态查询失败: %v", logfmt.Label(a.UID, a.Nickname), err)
		return 0
	}
	if !st.Pending() {
		return 0
	}
	credit, err := s.cfg.Upstream.ClaimCompensation(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 补偿领取失败（可领 %dc，截止 %s）: %v",
			logfmt.Label(a.UID, a.Nickname), st.CreditNum, st.EndTime, err)
		return 0
	}
	log.Printf("streak-bonus %s: 🎊 补偿领取 +%dc（截止 %s）",
		logfmt.Label(a.UID, a.Nickname), credit, st.EndTime)
	return credit
}
