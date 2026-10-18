// streak.go 连登管家：签到排程后自动检查连登兑换档位 → 可兑换即兑换 → 按抽奖次数抽奖。
//
// 背景（2026-09-12）：成长中心连登档位（7d/14d/28d）按连续登录天数解锁，兑换发
// credit/energy/补签卡/抽奖次数；抽奖次数只能从兑换获得。兑换按钮在 UI 上恒可点，
// 但未解锁时服务端 403「连续登录天数不足」——所以放在每日签到后跑一遍（幂等），
// 到天数那天自动完成「兑换 → 抽奖」闭环，无需人工盯。
package scheduler

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// RunStreakBonusNow 对所有可用账号执行连登兑换 + 抽奖（幂等：locked/无次数自动跳过）。
// 由签到排程（RunCheckinNow）末尾调用；也可面板手动触发。
func (s *Scheduler) RunStreakBonusNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			// D4 门控只覆盖 CN 成长体系（连登/补签/兑换/抽奖走 CN web base）。礼包与补偿是
			// billing 域、按 realm 走 base，国际号实测 200——一并跳过就会静默漏领国际区活动。
			s.claimGiftQuiet(a)
			s.claimCompensationQuiet(a)
			continue
		}
		if a.IsEnterprise() {
			continue // 企业版门控：无成长体系（成长端点上游 403）
		}
		s.streakBonusAccount(a)
	}
}

// streakBonusAccount 单账号：补签保连登 → 礼包/补偿 → 兑换所有已解锁档位 → 抽完所有 chances。
func (s *Scheduler) streakBonusAccount(a *auth.Auth) {
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	// 0. 补签保连登：本月漏签且有补签卡则从最近一天往回补（连续天数一断就要重攒 7 天）。
	// 补签成功会改连签天数：此时 makeupMissed 返回重查后的快照；重查失败也用旧快照继续兑换。
	full, err = s.makeupMissed(a, full)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 补签后重查连登失败（用旧快照继续）: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
	// 0.5 礼包 + 补偿：两者都走「先查状态 → 待领才领」（见 compensation.go）。
	// 盲领会把「无活动/已领过」打成假的「+0c」成功行，真失败反倒无痕。
	s.claimGiftQuiet(a)
	s.claimCompensationQuiet(a)
	statuses := map[string]string{
		"7d":  full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := s.cfg.Upstream.GrowthRedeemTier(a, tier.Tier); err != nil {
			// 未解锁（403）属预期，静默；其余记日志。
			log.Printf("WARN: streak-bonus %s: redeem %s: %v", logfmt.Label(a.UID, a.Nickname), tier.Tier, err)
			continue
		}
		log.Printf("streak-bonus %s: ★ 兑换 %s 档（+%dc +%de 卡×%d 抽奖×%d）",
			a.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// 抽奖：按当前 chances 全抽完（兑换刚发的次数已在服务端累加）。
	chances, err := s.cfg.Upstream.LotteryChances(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: lottery summary: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	for i := 0; i < chances; i++ {
		raw, err := s.cfg.Upstream.LotteryDraw(a)
		if err != nil {
			log.Printf("WARN: streak-bonus %s: draw: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 第%d抽 %s", logfmt.Label(a.UID, a.Nickname), i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: 抽奖完成 %d 次", logfmt.Label(a.UID, a.Nickname), chances)
	}
}

// compactJSON 裁剪奖品载荷（日志单行可读）。
func compactJSON(raw json.RawMessage) string {
	// logfmt.Truncate 回退到 UTF-8 rune 边界：奖品名是中文，按字节切会打出半个字。
	s := logfmt.Truncate(string(raw), 220)
	if len(raw) > 220 {
		s += "…"
	}
	return s
}

// makeupMissed 本月漏签且有补签卡时自动补签（保住连登连续天数）。
//
// 待补日期只看活跃日历：`/streak` 的 `makeup_dates` 是**已补登历史**，把它当待办会
// 反复提交补过的日子；日历缺项也不推测为断登。窗口 = max(当月 1 号（服务端 today）、
// launch_date、活动起始日 2026-06-17) → today 之前，score == 0，排除已补登日期。
//
// 返回供后续兑换使用的连登快照：没补签就复用传入的 full（同一轮不打两次 /streak）；
// 补过签则重查（连签天数变了，兑换档位要看最新值）。返回值始终非 nil，第二个返回值
// 只在「补签后重查失败」时非空——调用方提醒后可用旧快照继续，别让补签拖垮兑换。
//
// 失败提醒：无卡/无漏签属常态，静默；查询失败、日历异常、上游拒绝都是硬失败，必须
// 打 WARN（其他步骤成功不能盖掉它），并立即停止本轮——绝不继续凭猜测消耗补签卡。
func (s *Scheduler) makeupMissed(a *auth.Auth, full *upstream.StreakFull) (*upstream.StreakFull, error) {
	if full == nil {
		return nil, errors.New("补签判断缺少连登状态")
	}
	if full.MakeupCards.Balance <= 0 {
		return full, nil // 无卡：不额外查日历（省一次只读请求）
	}
	hm, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		log.Printf("WARN: streak-bonus %s: 补签判断查活跃日历失败: %v", logfmt.Label(a.UID, a.Nickname), err)
		return full, nil
	}
	broken, err := upstream.MakeupCandidates(full, hm)
	if err != nil {
		// 数据异常时猜窗口就是凭猜测花卡：本轮停手并提醒。
		log.Printf("WARN: streak-bonus %s: 补签日期不可判定（本轮不补）: %v", logfmt.Label(a.UID, a.Nickname), err)
		return full, nil
	}
	used := 0
	for i, n := 0, min(full.MakeupCards.Balance, len(broken)); i < n; i++ {
		d := broken[i] // MakeupCandidates 已按「最近优先」排序
		if err := s.cfg.Upstream.UseMakeupCard(a, d); err != nil {
			if upstream.IsMakeupNotNeeded(err) {
				// 查询后被客户端/另一轮补上：正常竞争，不是失败，也不再往前补。
				log.Printf("streak-bonus %s: %s 已活跃或已补签，无需再补", logfmt.Label(a.UID, a.Nickname), d)
				break
			}
			log.Printf("WARN: streak-bonus %s: 补签 %s 失败: %v", logfmt.Label(a.UID, a.Nickname), d, err)
			break
		}
		used++
		log.Printf("streak-bonus %s: ★ 已用补签卡补签 %s（保连登）", logfmt.Label(a.UID, a.Nickname), d)
	}
	if used == 0 {
		return full, nil
	}
	latest, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		return full, err
	}
	return latest, nil
}
