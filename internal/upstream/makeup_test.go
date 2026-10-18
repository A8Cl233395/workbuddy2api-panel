package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// makeupStreak 造一份连登状态（launch_date + 已补登历史）。
func makeupStreak(t *testing.T, launch string, madeUpRaw string) *StreakFull {
	t.Helper()
	raw := `{"launch_date":"` + launch + `","makeup_cards":{"balance":2},` +
		`"streak":{"days":3,"makeup_dates":` + madeUpRaw + `}}`
	s := &StreakFull{}
	if err := json.Unmarshal([]byte(raw), s); err != nil {
		t.Fatalf("连登状态解析失败: %v", err)
	}
	return s
}

// TestMakeupCandidatesExcludesMakeupHistory 回归 245f3138：makeup_dates 是**已补登历史**，
// 必须排除；待补日期只来自活跃日历中 score == 0 的历史日期，且最近的排最前。
func TestMakeupCandidatesExcludesMakeupHistory(t *testing.T) {
	// 9-28 已在补登历史里 → 不能提交；9-27 是真正的断登；9-30（今天）未结束不算。
	hm, err := ParseMakeupHeatmap([]byte(`{"today":{"date":"2026-09-30"},"cells":[
		{"date":"2026-09-27","score":0},
		{"date":"2026-09-28","score":0},
		{"date":"2026-09-29","score":12},
		{"date":"2026-09-30","score":0}]}`))
	if err != nil {
		t.Fatalf("ParseMakeupHeatmap: %v", err)
	}
	got, err := MakeupCandidates(makeupStreak(t, "2026-06-17", `["2026-09-28"]`), hm)
	if err != nil {
		t.Fatalf("MakeupCandidates: %v", err)
	}
	if len(got) != 1 || got[0] != "2026-09-27" {
		t.Fatalf("candidates=%v want [2026-09-27]（排除已补登 + 只取 0 分历史日）", got)
	}
	// 补登历史为空（首次补登）必须照常给出候选，不能被当成「无需补登」。
	got, err = MakeupCandidates(makeupStreak(t, "2026-06-17", `[]`), hm)
	if err != nil {
		t.Fatalf("MakeupCandidates: %v", err)
	}
	if len(got) != 2 || got[0] != "2026-09-28" || got[1] != "2026-09-27" {
		t.Fatalf("candidates=%v want [2026-09-28 2026-09-27]（最近优先）", got)
	}
}

// TestMakeupCandidatesWindow 窗口 = max(当月 1 号, launch_date, 活动起始日) → today 之前。
func TestMakeupCandidatesWindow(t *testing.T) {
	body := `{"today":{"date":"2026-09-30"},"cells":[
		{"date":"2026-08-31","score":0},
		{"date":"2026-09-01","score":0},
		{"date":"2026-09-20","score":0},
		{"date":"2026-09-30","score":0}]}`
	hm, err := ParseMakeupHeatmap([]byte(body))
	if err != nil {
		t.Fatalf("ParseMakeupHeatmap: %v", err)
	}
	cases := []struct {
		name    string
		launch  string
		want    []string
		wantErr bool
	}{
		{name: "默认窗口只补当月历史", launch: "2026-06-17", want: []string{"2026-09-20", "2026-09-01"}},
		{name: "上线日截断更早的断登", launch: "2026-09-10", want: []string{"2026-09-20"}},
		{name: "上线日晚于今天", launch: "2026-09-29", want: nil},
		{name: "缺上线日不猜窗口", launch: "", wantErr: true},
		{name: "上线日非法不猜窗口", launch: "2026-02-30", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MakeupCandidates(makeupStreak(t, tc.launch, `[]`), hm)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got candidates=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MakeupCandidates: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("candidates=%v want %v", got, tc.want)
			}
		})
	}
}

// TestMakeupCandidatesRejectsBrokenStructures 结构未知时宁可停下报错，也不能猜日期。
func TestMakeupCandidatesRejectsBrokenStructures(t *testing.T) {
	badHeatmaps := []string{
		`null`,
		`{}`,
		`{"today":{"date":"2026-09-30"}}`,
		`{"today":{"date":"2026-02-30"},"cells":[]}`,
		`{"today":{"date":"2026-09-30"},"cells":null}`,
		`{"today":{"date":"2026-09-30"},"cells":[null]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-28"}]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-28","score":"0"}]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-28","score":true}]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-28","score":-1}]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-31","score":0}]}`,
		`{"today":{"date":"2026-09-30"},"cells":[{"date":"2026-09-28","score":0},{"date":"2026-09-28","score":5}]}`,
	}
	for _, body := range badHeatmaps {
		hm, err := ParseMakeupHeatmap([]byte(body))
		if err == nil {
			if _, err = MakeupCandidates(makeupStreak(t, "2026-06-17", `[]`), hm); err == nil {
				t.Errorf("body=%s 应报错（不能把异常数据当成 0 分断登）", body)
			}
		}
	}

	// 已补登历史形状异常：同样必须报错。
	for _, madeUp := range []string{`null`, `"2026-09-01"`, `["2026-09-31"]`, `[{}]`, `[null]`} {
		hm, err := ParseMakeupHeatmap([]byte(`{"today":{"date":"2026-09-30"},"cells":[]}`))
		if err != nil {
			t.Fatalf("ParseMakeupHeatmap: %v", err)
		}
		if _, err := MakeupCandidates(makeupStreak(t, "2026-06-17", madeUp), hm); err == nil {
			t.Errorf("makeup_dates=%s 应报错", madeUp)
		}
	}
}

// TestMakeupCandidatesNoCellsNoGuess 日历里没有某一天就不推测它的状态。
func TestMakeupCandidatesNoCellsNoGuess(t *testing.T) {
	hm, err := ParseMakeupHeatmap([]byte(`{"today":{"date":"2026-09-30"},"cells":[]}`))
	if err != nil {
		t.Fatalf("ParseMakeupHeatmap: %v", err)
	}
	got, err := MakeupCandidates(makeupStreak(t, "2026-06-17", `["2026-09-01"]`), hm)
	if err != nil {
		t.Fatalf("MakeupCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("candidates=%v want 空（缺项不猜）", got)
	}
}

// TestIsMakeupNotNeeded 只认上游明确的「日期不须补登」文案（HTTP 400）。
func TestIsMakeupNotNeeded(t *testing.T) {
	yes := &Error{Status: 400, Msg: `{"code":400,"msg":"date is not broken, no makeup needed"}`}
	if !IsMakeupNotNeeded(yes) {
		t.Error("明确文案应判为良性竞争")
	}
	for _, err := range []error{
		nil,
		&Error{Status: 500, Msg: "date is not broken, no makeup needed"},
		&Error{Status: 400, Msg: `{"code":400,"msg":"date is not broken"}`},
		&Error{Status: 400, Msg: "invalid request"},
	} {
		if IsMakeupNotNeeded(err) {
			t.Errorf("err=%v 不应判为良性", err)
		}
	}
}
