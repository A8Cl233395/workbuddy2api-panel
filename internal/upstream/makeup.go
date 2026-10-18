// makeup.go 补登（补签）日期判断：活跃日历 → 待补日期。
//
// 背景（对齐上游 245f3138 的固定规则）：`/activity/growth/streak` 里的
// `streak.makeup_dates` 是**已补登历史**（官方 GrowthCenterPage 命中即显示「已补签」），
// 不是待办清单。把它当待补日期会反复提交已补过的日子；而补登历史为空时又会漏掉
// 真正的断登。真正的待补日期只能由活跃日历 `/activity/growth/heatmap` 推出。
//
// 官方 AllTasksPage 的 canMakeup 口径：同一自然月、今天之前、不早于上线日期
// `launch_date` 且不早于活动起始日 2026-06-17；缺上线日期时禁止补登。
// 日期边界取 heatmap.today（服务端日期），不读宿主机时钟——本机时区会把月初/午夜算错。
//
// 日历缺项不推测为零：只有明确给出 score == 0 的历史日期才算断登。结构异常、
// 记录冲突、分数非法都整段停止（调用方不得再消耗补登卡）。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	heatmapPath = "/activity/growth/heatmap"
	// makeupActivityStart 成长中心活动起始日（官方前端 canMakeup 的硬下界）。
	makeupActivityStart = "2026-06-17"
	// makeupNotNeededMarker 「查询后状态已变化」的明确拒绝文案（HTTP 400）。
	// 之前的客户端或另一轮把它补上了，属正常竞争，不该当失败刷日志。
	makeupNotNeededMarker = "date is not broken, no makeup needed"
	// isoDateLayout 只接受严格的 YYYY-MM-DD（time.Parse 同时拒绝 2026-02-30 这类假日期）。
	isoDateLayout = "2006-01-02"
)

// MakeupHeatmap 活跃日历（严格解析后的形状）。
type MakeupHeatmap struct {
	Today  time.Time          // heatmap.today.date（服务端日期）
	TodayS string             // 原始 today 字符串（日志/比对用）
	Scores map[string]float64 // YYYY-MM-DD → score
}

// GrowthHeatmap 拉取活跃日历；HTTP/业务码失败由 growthJSON 转成 *Error，
// 结构异常由 ParseMakeupHeatmap 拒绝。
func (c *Client) GrowthHeatmap(a *auth.Auth) (*MakeupHeatmap, error) {
	data, err := c.growthJSON(a, http.MethodGet, heatmapPath, nil)
	if err != nil {
		return nil, err
	}
	return ParseMakeupHeatmap(data)
}

// ParseMakeupHeatmap 严格解析活跃日历：today.date 必须存在且是有效 ISO 日期；
// cells 必须存在且每条都是 {date, score}，score 必须是非负有限数字，同一天不得冲突。
func ParseMakeupHeatmap(data json.RawMessage) (*MakeupHeatmap, error) {
	var raw struct {
		Today *struct {
			Date string `json:"date"`
		} `json:"today"`
		Cells *[]json.RawMessage `json:"cells"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("活跃日历格式无效: %w", err)
	}
	if raw.Today == nil {
		return nil, errors.New("活跃日历缺少服务端日期（today.date）")
	}
	today, err := parseISODate(raw.Today.Date)
	if err != nil {
		return nil, fmt.Errorf("活跃日历缺少服务端日期（today.date）: %w", err)
	}
	if raw.Cells == nil {
		return nil, errors.New("活跃日历缺少日期列表（cells）")
	}
	out := &MakeupHeatmap{Today: today, TodayS: raw.Today.Date, Scores: make(map[string]float64, len(*raw.Cells))}
	for _, cellRaw := range *raw.Cells {
		trimmed := bytes.TrimSpace(cellRaw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, errors.New("活跃日历记录格式无效")
		}
		var cell struct {
			Date  *string         `json:"date"`
			Score json.RawMessage `json:"score"`
		}
		if err := json.Unmarshal(trimmed, &cell); err != nil {
			return nil, fmt.Errorf("活跃日历记录格式无效: %w", err)
		}
		if cell.Date == nil {
			return nil, errors.New("活跃日历记录缺少日期")
		}
		day, err := parseISODate(*cell.Date)
		if err != nil {
			return nil, fmt.Errorf("活跃日历日期格式无效: %w", err)
		}
		score, err := parseScore(cell.Score)
		if err != nil {
			return nil, err
		}
		key := day.Format(isoDateLayout)
		if prev, ok := out.Scores[key]; ok && prev != score {
			return nil, fmt.Errorf("活跃日历同一日期的记录冲突（%s）", key)
		}
		out.Scores[key] = score
	}
	return out, nil
}

// MakeupCandidates 从连登状态与活跃日历选出**待补登**日期，最近的断点排在最前。
//
// 窗口 = [max(当月 1 号, launch_date, 活动起始日), today)，且 score == 0，
// 并排除 streak.makeup_dates（已补登历史）。缺 launch_date 或数据异常返回错误：
// 猜窗口等于凭猜测消耗补登卡，宁可整段停下提醒。
func MakeupCandidates(streak *StreakFull, hm *MakeupHeatmap) ([]string, error) {
	if hm == nil {
		return nil, errors.New("活跃日历缺失")
	}
	if streak == nil {
		return nil, errors.New("连登状态缺失")
	}
	if streak.LaunchDate == "" {
		return nil, errors.New("连登状态缺少活动上线日期（launch_date），不敢猜补登窗口")
	}
	launch, err := parseISODate(streak.LaunchDate)
	if err != nil {
		return nil, fmt.Errorf("连登状态上线日期格式无效: %w", err)
	}
	activityStart, _ := time.Parse(isoDateLayout, makeupActivityStart)
	monthStart := time.Date(hm.Today.Year(), hm.Today.Month(), 1, 0, 0, 0, 0, time.UTC)
	start := monthStart
	if launch.After(start) {
		start = launch
	}
	if activityStart.After(start) {
		start = activityStart
	}
	madeUp, err := streak.makeupDateSet()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hm.Scores))
	for key, score := range hm.Scores {
		if score != 0 || madeUp[key] {
			continue
		}
		day, err := parseISODate(key)
		if err != nil {
			return nil, fmt.Errorf("活跃日历日期格式无效: %w", err)
		}
		if day.Before(start) || !day.Before(hm.Today) {
			continue
		}
		out = append(out, key)
	}
	// 最近的断点优先：尽早恢复当前连登。
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

// makeupDateSet 解析已补登历史。缺字段 → 空集（首次补登不能被当成异常拦下）；
// 显式 null / 非数组 / 非法日期 → 错误（结构未知时不能猜）。
func (s *StreakFull) makeupDateSet() (map[string]bool, error) {
	set := map[string]bool{}
	raw := bytes.TrimSpace(s.Streak.MakeupDates)
	if len(raw) == 0 {
		return set, nil
	}
	if bytes.Equal(raw, []byte("null")) {
		return nil, errors.New("已补登记录格式无效（makeup_dates 为 null）")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("已补登记录格式无效: %w", err)
	}
	for _, item := range list {
		var v string
		if err := json.Unmarshal(item, &v); err != nil {
			return nil, fmt.Errorf("已补登记录格式无效: %w", err)
		}
		day, err := parseISODate(v)
		if err != nil {
			return nil, fmt.Errorf("已补登记录日期格式无效: %w", err)
		}
		set[day.Format(isoDateLayout)] = true
	}
	return set, nil
}

// IsMakeupNotNeeded 判定「查询后状态已变化/本就活跃」的良性拒绝：
// HTTP 400 + 明确文案。调用方按无需操作处理（不记失败、不再继续往前补）。
func IsMakeupNotNeeded(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), makeupNotNeededMarker)
}

// parseISODate 严格 ISO 日期（长度 10、YYYY-MM-DD、真实存在的一天）。
func parseISODate(v string) (time.Time, error) {
	if len(v) != len("2026-06-17") {
		return time.Time{}, fmt.Errorf("日期格式无效: %q", v)
	}
	d, err := time.Parse(isoDateLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("日期格式无效: %q", v)
	}
	return d, nil
}

// parseScore 只接受明确的非负有限数字（字符串/布尔/null/负数/超范围都拒绝）。
func parseScore(raw json.RawMessage) (float64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, errors.New("活跃日历分数无效（缺少 score）")
	}
	// 必须是 JSON 数字字面量：字符串 "0"、布尔、对象都不能被当成 0 分断登。
	if c := trimmed[0]; (c < '0' || c > '9') && c != '-' {
		return 0, fmt.Errorf("活跃日历分数无效: %s", trimmed)
	}
	var num json.Number
	if err := json.Unmarshal(trimmed, &num); err != nil {
		return 0, fmt.Errorf("活跃日历分数无效: %w", err)
	}
	f, err := num.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, fmt.Errorf("活跃日历分数无效: %s", trimmed)
	}
	return f, nil
}
