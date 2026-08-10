package conf

import (
	"fmt"
	"testing"
)

// reset 清理指定网络的统计状态，避免用例间互相污染
func reset(net string) {
	data.Delete(net)
	last.Delete(net)
}

func TestRecentRateReflectsLatestSamples(t *testing.T) {
	const net = "test-recent"
	reset(net)

	// 先灌满 30 次成功，再来 10 次失败
	for i := 0; i < 30; i++ {
		RecordSuccess(net, "1")
	}
	for i := 0; i < 10; i++ {
		RecordFailure(net)
	}

	s := getStat(net)
	s.mu.RLock()
	recent, n := s.recentRatef()
	total := s.ratef()
	s.mu.RUnlock()

	// 近期窗口 20 条 = 10 成功 + 10 失败 → 50%
	if n != recentRecords {
		t.Fatalf("近期窗口样本数 = %d, 期望 %d", n, recentRecords)
	}
	if recent != 50 {
		t.Fatalf("近期成功率 = %.2f, 期望 50.00", recent)
	}

	// 全窗口 40 条 = 30 成功 → 75%
	if total != 75 {
		t.Fatalf("全窗口成功率 = %.2f, 期望 75.00", total)
	}
}

func TestRecentRateAfterWraparound(t *testing.T) {
	const net = "test-wrap"
	reset(net)

	// 超过 maxRecords 触发环形回绕
	for i := 0; i < maxRecords+50; i++ {
		RecordSuccess(net, "1")
	}
	for i := 0; i < 5; i++ {
		RecordFailure(net)
	}

	s := getStat(net)
	s.mu.RLock()
	recent, n := s.recentRatef()
	s.mu.RUnlock()

	if n != recentRecords {
		t.Fatalf("回绕后近期窗口样本数 = %d, 期望 %d", n, recentRecords)
	}

	// 近期 20 条 = 15 成功 + 5 失败 → 75%
	if recent != 75 {
		t.Fatalf("回绕后近期成功率 = %.2f, 期望 75.00", recent)
	}
}

// 回归用例：修复前 RecordFailure 不更新 last，节点持续故障时成功率会冻结在最后一次成功的数值
func TestFailureRefreshesReportedRate(t *testing.T) {
	const net = "test-freeze"
	reset(net)

	for i := 0; i < 10; i++ {
		RecordSuccess(net, "100")
	}

	if got := GetStats()[net].Succ; got != "100.00%" {
		t.Fatalf("全成功时上报成功率 = %s, 期望 100.00%%", got)
	}

	for i := 0; i < 10; i++ {
		RecordFailure(net)
	}

	itm := GetStats()[net]
	if itm.Succ != "50.00%" {
		t.Fatalf("失败后上报成功率 = %s, 期望 50.00%%（修复前会冻结为 100.00%%）", itm.Succ)
	}
	if itm.Fail == 0 {
		t.Fatal("失败时间戳未记录")
	}
	if itm.Block != "100" {
		t.Fatalf("失败不应清空最后成功高度, got %q", itm.Block)
	}
}

func TestMetricsStaleAndNeverSucceeded(t *testing.T) {
	const net = "test-never"
	reset(net)

	RecordFailure(net)

	var found bool
	for _, m := range GetMetrics() {
		if m.Network != net {
			continue
		}
		found = true

		if m.StaleSeconds != -1 {
			t.Fatalf("从未成功时 stale_seconds = %d, 期望 -1", m.StaleSeconds)
		}
		if m.LastSuccessAt != 0 {
			t.Fatalf("从未成功时 last_success_at = %d, 期望 0", m.LastSuccessAt)
		}
		if m.SuccessRate != 0 {
			t.Fatalf("仅失败时成功率 = %.2f, 期望 0", m.SuccessRate)
		}
		if m.Failed != 1 || m.Total != 1 {
			t.Fatalf("样本统计异常 failed=%d total=%d", m.Failed, m.Total)
		}
	}

	if !found {
		t.Fatal("GetMetrics 未包含仅失败过的网络")
	}
}

func TestMetricsSortedStable(t *testing.T) {
	for _, n := range []string{"zeta", "alpha", "mid"} {
		reset(n)
		RecordSuccess(n, "1")
	}
	defer func() {
		for _, n := range []string{"zeta", "alpha", "mid"} {
			reset(n)
		}
	}()

	var first string
	for i := 0; i < 5; i++ {
		var order string
		for _, m := range GetMetrics() {
			order += m.Network + ","
		}
		if i == 0 {
			first = order

			continue
		}
		if order != first {
			t.Fatalf("多次调用顺序不一致：%s != %s", order, first)
		}
	}

	if first == "" {
		t.Fatal("未取到指标")
	}
	fmt.Println("稳定顺序:", first)
}
