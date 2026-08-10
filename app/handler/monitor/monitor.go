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
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/spf13/cast"
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
)

type metric struct {
	conf.Metric
	Alert  bool     `json:"alert"`
	Reason []string `json:"reason,omitempty"`
}

// Stats GET /api/monitor/stats
//
// 鉴权：token 查询参数，或 X-Monitor-Token / Authorization 请求头。
// 可选参数：
//
//	threshold  成功率告警阈值，默认 50
//	stale      同步陈旧告警阈值（秒），默认 300，传 0 关闭该维度
//	window     成功率取样窗口，recent（默认，近期窗口，灵敏）或 total（全窗口 1000 样本）
func (Monitor) Stats(ctx *gin.Context) {
	if !authorize(ctx) {
		ctx.JSON(http.StatusForbidden, gin.H{"code": 403, "msg": "invalid monitor token"})

		return
	}

	var rateThreshold = defaultRateThreshold
	if v := ctx.Query("threshold"); v != "" {
		rateThreshold = cast.ToFloat64(v)
	}

	var staleThreshold = int64(defaultStaleThreshold)
	if v := ctx.Query("stale"); v != "" {
		staleThreshold = cast.ToInt64(v)
	}

	var useTotal = ctx.Query("window") == "total"

	var items = make([]metric, 0)
	var alerts = make([]string, 0)
	var scanning int

	for _, itm := range conf.GetMetrics() {
		itm.Scanning = task.ScanActive(itm.Network)
		if itm.Scanning {
			scanning++
		}

		var m = metric{Metric: itm, Reason: make([]string, 0)}

		var rate = itm.RecentRate
		if useTotal {
			rate = itm.SuccessRate
		}

		// 样本为空时成功率恒为 100，不具备判断意义，跳过以免掩盖问题或误报
		if itm.Total > 0 && rate < rateThreshold {
			m.Alert = true
			m.Reason = append(m.Reason, fmt.Sprintf("成功率 %.2f%% 低于阈值 %.2f%%", rate, rateThreshold))
		}

		// 扫块为需求驱动，空闲停扫时陈旧属正常，仅在应活跃时判断该维度
		if staleThreshold > 0 && itm.Scanning && itm.StaleSeconds > staleThreshold {
			m.Alert = true
			m.Reason = append(m.Reason, fmt.Sprintf("已 %d 秒未成功同步，超过阈值 %d 秒", itm.StaleSeconds, staleThreshold))
		}

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

	ctx.JSON(http.StatusOK, gin.H{
		"code":   200,
		"status": status,
		"alert":  len(alerts) > 0,
		"alerts": alerts,
		"summary": gin.H{
			"networks":         len(items),
			"scanning":         scanning,
			"alerting":         len(alerts),
			"rate_threshold":   rateThreshold,
			"stale_threshold":  staleThreshold,
			"window":           map[bool]string{true: "total", false: "recent"}[useTotal],
			"scan_demand_note": "扫块为需求驱动，无待处理订单且未启用钱包监控时会停扫，此时 scanning 为 false，陈旧维度不参与告警",
		},
		"networks": items,
	})
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
