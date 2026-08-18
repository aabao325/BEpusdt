package monitor

import (
	"net/url"
	"testing"

	"github.com/v03413/bepusdt/app/conf"
)

// queryOf 用真实的 URL 解析构造 query 取值函数，避免手写 map 掩盖编码问题
func queryOf(rawQuery string) func(string) string {
	values, _ := url.ParseQuery(rawQuery)

	return values.Get
}

func TestParseParamsNormal(t *testing.T) {
	p := parseParams(queryOf("token=X&threshold=90&stale=0&window=total"))

	if p.RateThreshold != 90 {
		t.Fatalf("threshold = %v, 期望 90", p.RateThreshold)
	}
	if p.StaleThreshold != 0 {
		t.Fatalf("stale = %v, 期望 0", p.StaleThreshold)
	}
	if !p.UseTotal {
		t.Fatal("window=total 未生效")
	}
	if len(p.Errors) != 0 {
		t.Fatalf("不应有参数错误: %v", p.Errors)
	}
}

// 关键回归：URL 里的 & 被 HTML 转义成 &amp; 时，参数名会变成 amp;threshold，
// threshold 取不到值而静默回落 50，表现为「设了 90 却按 50 告警」
func TestParseParamsHtmlEscapedAmpersand(t *testing.T) {
	p := parseParams(queryOf("token=X&amp;threshold=90&amp;stale=0&amp;window=total"))

	if p.RateThreshold != defaultRateThreshold {
		t.Fatalf("threshold = %v, 期望回落到默认 %v", p.RateThreshold, defaultRateThreshold)
	}
	if p.UseTotal {
		t.Fatal("window 也应未生效")
	}

	// 这是本次修复的意义所在：能从响应里看出参数没生效
	t.Logf("参数被转义后 threshold 回落为 %v，window=recent", p.RateThreshold)
}

func TestParseParamsInvalidValuesReported(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"非数值", "threshold=abc"},
		{"超范围", "threshold=900"},
		{"负数", "threshold=-1"},
		{"stale 非整数", "stale=1.5"},
		{"stale 负数", "stale=-10"},
		{"window 无效", "window=daily"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := parseParams(queryOf(c.query))
			if len(p.Errors) == 0 {
				t.Fatalf("%s 应产生参数错误提示", c.query)
			}
			t.Log(p.Errors[0])
		})
	}
}

func TestParseParamsSamples(t *testing.T) {
	p := parseParams(queryOf("threshold=90&samples=100"))
	if p.Samples != 100 {
		t.Fatalf("samples = %d, 期望 100", p.Samples)
	}
	if len(p.Errors) != 0 {
		t.Fatalf("不应有错误: %v", p.Errors)
	}

	for _, bad := range []string{"samples=0", "samples=1001", "samples=abc", "samples=-5"} {
		p := parseParams(queryOf(bad))
		if len(p.Errors) == 0 {
			t.Fatalf("%s 应报错", bad)
		}
		if p.Samples != defaultSamples {
			t.Fatalf("%s 应回落默认 %d, got %d", bad, defaultSamples, p.Samples)
		}
	}
}

func TestParseParamsDefaults(t *testing.T) {
	p := parseParams(queryOf("token=X"))

	if p.RateThreshold != 50 {
		t.Fatalf("默认 threshold = %v, 期望 50", p.RateThreshold)
	}
	if p.StaleThreshold != 300 {
		t.Fatalf("默认 stale = %v, 期望 300", p.StaleThreshold)
	}
	if p.UseTotal {
		t.Fatal("默认应为 recent 窗口")
	}
	if p.Samples != 20 {
		t.Fatalf("默认 samples = %d, 期望 20", p.Samples)
	}
}

// needed 返回窗口被成功样本填满后，需要多少次连续失败才能使成功率跌破阈值
func needed(windowSize int, threshold float64) int {
	for fail := 1; fail <= windowSize; fail++ {
		if float64(windowSize-fail)/float64(windowSize)*100 < threshold {
			return fail
		}
	}

	return -1
}

// 说明性用例：窗口大小直接决定告警灵敏度，也决定窗口覆盖多长时间。
// 这解释了为什么 window=total 配 threshold=90 会「看起来很久才告警」。
func TestFailuresNeededToCrossThreshold(t *testing.T) {
	if got := needed(20, 90); got != 3 {
		t.Fatalf("samples=20 跌破 90%% 需 %d 次失败, 期望 3", got)
	}
	if got := needed(100, 90); got != 11 {
		t.Fatalf("samples=100 跌破 90%% 需 %d 次失败, 期望 11", got)
	}
	if got := needed(1000, 90); got != 101 {
		t.Fatalf("total(1000) 跌破 90%% 需 %d 次失败, 期望 101", got)
	}

	// 各链 syncBlocksForward 的实际间隔（秒）
	intervals := map[string]int{"tron/xlayer": 3, "bsc/base/polygon/solana": 5, "ethereum": 12}

	for _, w := range []int{20, 100, 1000} {
		n := needed(w, 90)
		t.Logf("--- samples=%d, 阈值 90%%: 需 %d 次连续失败 ---", w, n)
		for name, sec := range intervals {
			t.Logf("    %-24s 告警耗时 ≈ %3d 秒, 窗口覆盖 ≈ %5.1f 分钟",
				name, n*sec, float64(w*sec)/60)
		}
	}
}

// 关键结论验证：5 分钟轮询下，samples 必须让窗口覆盖时长 ≥ 轮询间隔，
// 否则两次轮询之间自愈的故障会被漏掉。最快的链是 3 秒一轮。
func TestSamplesCoversPollInterval(t *testing.T) {
	const pollSeconds = 300 // changedetection 每 5 分钟一次
	const fastestScanSeconds = 3

	// 默认 20 样本在最快链上只覆盖 60 秒，存在盲区
	if covered := defaultSamples * fastestScanSeconds; covered >= pollSeconds {
		t.Fatalf("默认窗口覆盖 %d 秒，本用例前提是它小于轮询间隔 %d 秒", covered, pollSeconds)
	}

	// samples=100 恰好覆盖 300 秒，无盲区
	const recommended = 100
	if covered := recommended * fastestScanSeconds; covered < pollSeconds {
		t.Fatalf("samples=%d 覆盖 %d 秒，不足轮询间隔 %d 秒", recommended, covered, pollSeconds)
	}

	t.Logf("默认 samples=%d 在 3 秒链上仅覆盖 %d 秒 → 5 分钟轮询有 %d 秒盲区",
		defaultSamples, defaultSamples*fastestScanSeconds, pollSeconds-defaultSamples*fastestScanSeconds)
	t.Logf("samples=%d 覆盖 %d 秒 → 恰好无盲区，且需 %d 次失败才告警（约 %d 秒）",
		recommended, recommended*fastestScanSeconds, needed(recommended, 90), needed(recommended, 90)*fastestScanSeconds)
}

// 空闲停扫的网络绝不告警：这是本次修复的核心。
// 扫块需求驱动，无订单时统计值冻结，此时任何告警都不可行动。
func TestEvaluateSkipsIdleNetwork(t *testing.T) {
	// 一条既低成功率、又长期未同步的网络，但已停扫
	idle := conf.Metric{
		Network:      "bsc",
		Scanning:     false,
		Total:        100,
		RecentRate:   20,
		SuccessRate:  20,
		StaleSeconds: 513813,
	}

	m := evaluate(idle, false, 90, 300)
	if m.Alert {
		t.Fatalf("空闲网络不应告警, reason=%v", m.Reason)
	}
	if len(m.Reason) != 0 {
		t.Fatalf("空闲网络不应有告警原因, got %v", m.Reason)
	}
}

// 同样的数据，只要在扫链就必须告警
func TestEvaluateAlertsWhenScanning(t *testing.T) {
	active := conf.Metric{
		Network:      "bsc",
		Scanning:     true,
		Total:        100,
		RecentRate:   20,
		SuccessRate:  20,
		StaleSeconds: 10,
	}

	m := evaluate(active, false, 90, 300)
	if !m.Alert {
		t.Fatal("扫链中且成功率 20% < 90% 应告警")
	}
	if m.RateUsed != 20 {
		t.Fatalf("rate_used = %v, 期望 20", m.RateUsed)
	}
	if len(m.Reason) != 1 {
		t.Fatalf("应只有成功率一条原因, got %v", m.Reason)
	}
	t.Log(m.Reason[0])
}

// stale=0 关闭陈旧维度后，只剩成功率一个告警来源
func TestEvaluateStaleDisabled(t *testing.T) {
	itm := conf.Metric{
		Network:      "bsc",
		Scanning:     true,
		Total:        100,
		RecentRate:   100,
		SuccessRate:  100,
		StaleSeconds: 999999,
	}

	if m := evaluate(itm, false, 90, 0); m.Alert {
		t.Fatalf("stale=0 且成功率达标时不应告警, reason=%v", m.Reason)
	}

	// 未关闭时应告警，证明用例本身有效
	if m := evaluate(itm, false, 90, 300); !m.Alert {
		t.Fatal("stale=300 时长期未同步应告警（对照组）")
	}
}

// 样本为空时成功率恒为 100，不参与判断，避免刚启动就误报
func TestEvaluateNoSamples(t *testing.T) {
	itm := conf.Metric{Network: "bsc", Scanning: true, Total: 0, RecentRate: 100, StaleSeconds: 5}
	if m := evaluate(itm, false, 90, 300); m.Alert {
		t.Fatalf("无样本时不应告警, reason=%v", m.Reason)
	}
}

// window=total 时应改用全窗口成功率参与比较
func TestEvaluateUsesTotalWindow(t *testing.T) {
	itm := conf.Metric{
		Network:     "bsc",
		Scanning:    true,
		Total:       1000,
		RecentRate:  20, // 近期很差
		SuccessRate: 95, // 长期尚可
	}

	if m := evaluate(itm, true, 90, 0); m.Alert {
		t.Fatalf("window=total 应看 95%%，不应告警, rate_used=%v", m.RateUsed)
	}
	if m := evaluate(itm, false, 90, 0); !m.Alert {
		t.Fatal("window=recent 应看 20%，应告警")
	}
}

// 告警文案必须是干净的单个百分号，不能出现重复数字
func TestEvaluateReasonFormat(t *testing.T) {
	itm := conf.Metric{Network: "bsc", Scanning: true, Total: 100, RecentRate: 50}
	m := evaluate(itm, false, 90, 0)

	const want = "成功率 50.00% 低于阈值 90.00%"
	if len(m.Reason) != 1 || m.Reason[0] != want {
		t.Fatalf("告警文案 = %q, 期望 %q", m.Reason, want)
	}
}
