package conf

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

const maxRecords = 1000

// recentRecords 近期窗口样本数。maxRecords 窗口要跌破 50% 需累积约 500 次失败，
// 对告警而言过于迟钝，故额外提供一个短窗口反映节点当前健康度。
const recentRecords = 20

type stat struct {
	mu      sync.RWMutex
	records []bool
	index   int // 当前位置
	total   int // 已记录总数
	succ    int // 成功记录数
}

type info struct {
	Block string `json:"block"`
	Succ  string `json:"succ"`
	Time  int64  `json:"time"` // 最后一次成功同步的时间戳
	Fail  int64  `json:"fail"` // 最后一次同步失败的时间戳
}

// Metric 节点监控指标，用于外部监控系统告警判断
type Metric struct {
	Network       string  `json:"network"`
	Block         string  `json:"block"`
	SuccessRate   float64 `json:"success_rate"`    // 全窗口成功率数值，便于阈值比较
	Succ          string  `json:"succ"`            // 全窗口成功率百分比字符串，与后台展示一致
	RecentRate    float64 `json:"recent_rate"`     // 近期窗口成功率，对故障更灵敏
	RecentTotal   int     `json:"recent_total"`    // 近期窗口实际样本数
	Total         int     `json:"total"`           // 全窗口样本总数
	Failed        int     `json:"failed"`          // 全窗口失败数
	LastSuccessAt int64   `json:"last_success_at"` // 最后成功同步时间戳，0 表示从未成功
	LastFailureAt int64   `json:"last_failure_at"` // 最后失败时间戳，0 表示从未失败
	StaleSeconds  int64   `json:"stale_seconds"`   // 距最后成功同步的秒数，-1 表示从未成功
	Scanning      bool    `json:"scanning"`        // 扫块器当前是否应处于活跃状态
}

var (
	data sync.Map // map[string]*stat
	last sync.Map
)

func getStat(net string) *stat {
	val, _ := data.LoadOrStore(net, &stat{
		records: make([]bool, maxRecords),
	})
	return val.(*stat)
}

func RecordSuccess(net, block string) {
	s := getStat(net)
	s.mu.Lock()
	if s.total >= maxRecords && !s.records[s.index] {
		s.succ++
	} else if s.total < maxRecords {
		s.succ++
	}

	s.records[s.index] = true
	s.index = (s.index + 1) % maxRecords
	if s.total < maxRecords {
		s.total++
	}
	rate := s.rate()
	s.mu.Unlock()

	var prev info
	if v, ok := last.Load(net); ok {
		prev = v.(info)
	}

	last.Store(net, info{Block: block, Succ: rate, Time: time.Now().Unix(), Fail: prev.Fail})
}

func RecordFailure(net string) {
	s := getStat(net)
	s.mu.Lock()
	if s.total >= maxRecords && s.records[s.index] {
		s.succ--
	}

	s.records[s.index] = false
	s.index = (s.index + 1) % maxRecords
	if s.total < maxRecords {
		s.total++
	}
	rate := s.rate()
	s.mu.Unlock()

	// 失败同样刷新成功率，否则节点持续故障时后台与监控接口都会停留在最后一次成功的数值
	var prev info
	if v, ok := last.Load(net); ok {
		prev = v.(info)
	}

	last.Store(net, info{Block: prev.Block, Succ: rate, Time: prev.Time, Fail: time.Now().Unix()})
}

func GetStats() map[string]info {
	var m = make(map[string]info)
	last.Range(func(k, v interface{}) bool {
		m[k.(string)] = v.(info)

		return true
	})

	return m
}

// GetMetrics 返回全部已产生扫块记录的网络指标，按网络名排序保证输出稳定。
//
// recentWindow 指定「近期窗口」取多少个最新样本，用于计算 RecentRate。
// 使用方应让它与自身的轮询间隔相匹配：窗口覆盖的时间短于轮询间隔时，
// 两次轮询之间的故障若已自愈就会被漏掉。传 0 或负数则用默认值。
func GetMetrics(recentWindow int) []Metric {
	if recentWindow <= 0 {
		recentWindow = recentRecords
	}
	if recentWindow > maxRecords {
		recentWindow = maxRecords
	}

	var list = make([]Metric, 0)

	data.Range(func(k, v interface{}) bool {
		net := k.(string)
		s := v.(*stat)

		s.mu.RLock()
		total, succ := s.total, s.succ
		rate := s.ratef()
		recentRate, recentTotal := s.recentRatef(recentWindow)
		s.mu.RUnlock()

		var m = Metric{
			Network:      net,
			SuccessRate:  rate,
			Succ:         fmt.Sprintf("%.2f%%", rate),
			RecentRate:   recentRate,
			RecentTotal:  recentTotal,
			Total:        total,
			Failed:       total - succ,
			StaleSeconds: -1,
		}

		if v, ok := last.Load(net); ok {
			itm := v.(info)
			m.Block = itm.Block
			m.LastSuccessAt = itm.Time
			m.LastFailureAt = itm.Fail
			if itm.Time > 0 {
				m.StaleSeconds = time.Now().Unix() - itm.Time
			}
		}

		list = append(list, m)

		return true
	})

	sort.Slice(list, func(i, j int) bool { return list[i].Network < list[j].Network })

	return list
}

func GetSuccessRate(net string) string {
	s := getStat(net)
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.rate()
}

// rate 调用方必须持有锁
func (s *stat) rate() string {

	return fmt.Sprintf("%.2f%%", s.ratef())
}

// ratef 调用方必须持有锁
func (s *stat) ratef() float64 {
	if s.total == 0 {

		return 100
	}

	return float64(s.succ) / float64(s.total) * 100
}

// recentRatef 近期窗口成功率，从环形缓冲区末尾往回取样 window 个；调用方必须持有锁
func (s *stat) recentRatef(window int) (float64, int) {
	var n = window
	if s.total < n {
		n = s.total
	}
	if n == 0 {

		return 100, 0
	}

	var succ int
	for i := 1; i <= n; i++ {
		// index 指向下一个待写入位置，故最近一条为 index-1
		idx := (s.index - i + maxRecords) % maxRecords
		if s.records[idx] {
			succ++
		}
	}

	return float64(succ) / float64(n) * 100, n
}
