package monitor

import (
	"net/url"
	"testing"
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
