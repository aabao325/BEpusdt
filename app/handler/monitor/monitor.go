// Package monitor 提供面向外部监控系统（如 changedetection、Uptime Kuma）的只读节点状态接口。
//
// 与后台 /api/conf/rpc 的区别：
//  1. 使用数据库持久化的独立令牌鉴权，不依赖 24 小时内存会话，进程重启后依然有效；
//  2. 不返回任何 RPC 端点与 API Key，避免凭据落入监控系统的历史快照；
//  3. 直接给出 alert 布尔值与 status 关键字，便于外部按关键字触发告警。
package monitor

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/task"
)

type Monitor struct {
}

const (
	statusOk       = "OK"
	statusDegraded = "DEGRADED"
	statusNoData   = "NO_DATA"

	defaultRateThreshold  = 50.0 // 成功率告警阈值，百分比
	defaultStaleThreshold = 300  // 同步陈旧告警阈值，秒
	defaultSamples        = 20   // 近期窗口默认样本数
	maxSamples            = 1000 // 近期窗口上限，与 conf 层环形缓冲区容量一致
)

type metric struct {
	conf.Metric
	RateUsed float64  `json:"rate_used"` // 实际参与阈值比较的成功率，取决于 window 参数
	Alert    bool     `json:"alert"`
	Reason   []string `json:"reason,omitempty"`
}

// Stats GET /api/monitor/stats
//
// 鉴权：token 查询参数，或 X-Monitor-Token / Authorization 请求头。
// 可选参数：
//
//	threshold  成功率告警阈值，默认 50
//	stale      同步陈旧告警阈值（秒），默认 300，传 0 关闭该维度（仅在扫链中判定）
//	window     成功率取样窗口，recent（默认，近期窗口，灵敏）或 total（全窗口 1000 样本）
func (Monitor) Stats(ctx *gin.Context) {
	if !authorize(ctx) {
		ctx.JSON(http.StatusForbidden, gin.H{"code": 403, "msg": "invalid monitor token"})

		return
	}

	var p = parseParams(ctx.Query)
	var rateThreshold, staleThreshold, useTotal = p.RateThreshold, p.StaleThreshold, p.UseTotal

	var items = make([]metric, 0)
	var alerts = make([]string, 0)
	var scanning int

	for _, itm := range conf.GetMetrics(p.Samples) {
		itm.Scanning = task.ScanActive(itm.Network)
		if itm.Scanning {
			scanning++
		}

		var m = evaluate(itm, useTotal, rateThreshold, staleThreshold)

		if m.Alert {
			alerts = append(alerts, fmt.Sprintf("%s: %s", itm.Network, strings.Join(m.Reason, "；")))
		}

		items = append(items, m)
	}

	// 无任何样本时不能报 OK，否则与「全部健康」无法区分。
	// 该状态在网关空闲（无待处理订单且未启用钱包监控）时属正常，故不置 alert，
	// 交由使用方决定是否对 status 变化告警。
	var status = statusOk
	switch {
	case len(alerts) > 0:
		status = statusDegraded
	case len(items) == 0:
		status = statusNoData
	}

	var summary = gin.H{
		"networks":         len(items),
		"scanning":         scanning,
		"alerting":         len(alerts),
		"rate_threshold":   rateThreshold,
		"stale_threshold":  staleThreshold,
		"window":           map[bool]string{true: "total", false: "recent"}[useTotal],
		"recent_samples":   p.Samples,
		"scan_demand_note": "扫块为需求驱动，无待处理订单且未启用钱包监控时会停扫，此时 scanning 为 false，该网络完全不参与告警判定",
	}

	// 参数写错时显式回报，避免使用方以为阈值已生效
	if len(p.Errors) > 0 {
		summary["param_errors"] = p.Errors
	}

	ctx.JSON(http.StatusOK, gin.H{
		"code":     200,
		"status":   status,
		"alert":    len(alerts) > 0,
		"alerts":   alerts,
		"summary":  summary,
		"networks": items,
	})
}

// evaluate 判定单条网络是否需要告警。
//
// 前提：扫块为需求驱动，网关无待处理订单且未启用钱包监控时扫块器会主动停扫，
// 统计值随之冻结在停扫前的状态。此时无论成功率还是同步时间都不反映节点健康度，
// 告警既不可行动也无从恢复，故整条网络直接跳过判定——只在真正扫链时才判断。
func evaluate(itm conf.Metric, useTotal bool, rateThreshold float64, staleThreshold int64) metric {
	var rate = itm.RecentRate
	if useTotal {
		rate = itm.SuccessRate
	}

	var m = metric{Metric: itm, RateUsed: rate, Reason: make([]string, 0)}
	if !itm.Scanning {

		return m
	}

	// 样本为空时成功率恒为 100，不具备判断意义，跳过以免掩盖问题或误报
	if itm.Total > 0 && rate < rateThreshold {
		m.Alert = true
		m.Reason = append(m.Reason, fmt.Sprintf("成功率 %.2f%% 低于阈值 %.2f%%", rate, rateThreshold))
	}

	if staleThreshold > 0 && itm.StaleSeconds > staleThreshold {
		m.Alert = true
		m.Reason = append(m.Reason, fmt.Sprintf("已 %d 秒未成功同步，超过阈值 %d 秒", itm.StaleSeconds, staleThreshold))
	}

	return m
}

type params struct {
	RateThreshold  float64
	StaleThreshold int64
	UseTotal       bool
	Samples        int
	Errors         []string
}

// parseParams 解析查询参数。非法值不静默回落默认值，而是记入 Errors 一并返回，
// 否则使用方（如把 URL 里 & 误写成 &amp;）会看到「阈值明明设了 90 却按 50 告警」
// 这种无从排查的现象。
func parseParams(query func(string) string) params {
	var p = params{
		RateThreshold:  defaultRateThreshold,
		StaleThreshold: defaultStaleThreshold,
		Samples:        defaultSamples,
		Errors:         make([]string, 0),
	}

	if v := strings.TrimSpace(query("threshold")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		switch {
		case err != nil:
			p.Errors = append(p.Errors, fmt.Sprintf("threshold=%q 无法解析为数值，已按默认 %.0f 处理", v, defaultRateThreshold))
		case f < 0 || f > 100:
			p.Errors = append(p.Errors, fmt.Sprintf("threshold=%v 超出 0~100 范围，已按默认 %.0f 处理", f, defaultRateThreshold))
		default:
			p.RateThreshold = f
		}
	}

	if v := strings.TrimSpace(query("stale")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		switch {
		case err != nil:
			p.Errors = append(p.Errors, fmt.Sprintf("stale=%q 无法解析为整数，已按默认 %d 处理", v, defaultStaleThreshold))
		case n < 0:
			p.Errors = append(p.Errors, fmt.Sprintf("stale=%d 不能为负，已按默认 %d 处理", n, defaultStaleThreshold))
		default:
			p.StaleThreshold = n
		}
	}

	if v := strings.TrimSpace(query("samples")); v != "" {
		n, err := strconv.Atoi(v)
		switch {
		case err != nil:
			p.Errors = append(p.Errors, fmt.Sprintf("samples=%q 无法解析为整数，已按默认 %d 处理", v, defaultSamples))
		case n < 1 || n > maxSamples:
			p.Errors = append(p.Errors, fmt.Sprintf("samples=%d 超出 1~%d 范围，已按默认 %d 处理", n, maxSamples, defaultSamples))
		default:
			p.Samples = n
		}
	}

	if v := strings.TrimSpace(query("window")); v != "" {
		switch v {
		case "total":
			p.UseTotal = true
		case "recent":
			p.UseTotal = false
		default:
			p.Errors = append(p.Errors, fmt.Sprintf("window=%q 无效，仅支持 recent 或 total，已按 recent 处理", v))
		}
	}

	return p
}

func authorize(ctx *gin.Context) bool {
	var token = model.MonitorToken()
	if token == "" {

		return false
	}

	var input = ctx.Query("token")
	if input == "" {
		input = ctx.GetHeader("X-Monitor-Token")
	}
	if input == "" {
		// 兼容 Bearer 前缀
		input = strings.TrimPrefix(ctx.GetHeader("Authorization"), "Bearer ")
	}
	if input == "" {

		return false
	}

	return subtle.ConstantTimeCompare([]byte(token), []byte(strings.TrimSpace(input))) == 1
}
