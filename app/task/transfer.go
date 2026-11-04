package task

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/smallnest/chanx"
	"github.com/v03413/bepusdt/app/conf"
	"github.com/v03413/bepusdt/app/log"
	"github.com/v03413/bepusdt/app/model"
	"github.com/v03413/bepusdt/app/notifier"
	"github.com/v03413/bepusdt/app/task/notify"
	"github.com/v03413/tronprotocol/core"
)

type transfer struct {
	Network     string          `json:"network"`
	TxHash      string          `json:"tx_hash"`
	Amount      decimal.Decimal `json:"amount"`
	FromAddress string          `json:"from_address"`
	RecvAddress string          `json:"recv_address"`
	Timestamp   time.Time       `json:"timestamp"`
	TradeType   model.TradeType `json:"trade_type"`
	BlockNum    int             `json:"block_num"`
}

type resource struct {
	ID           string
	Type         core.Transaction_Contract_ContractType
	Balance      int64
	FromAddress  string
	RecvAddress  string
	Timestamp    time.Time
	ResourceCode core.ResourceCode
}

var resourceQueue = chanx.NewUnboundedChan[[]resource](context.Background(), 30) // 资源队列
var notOrderQueue = chanx.NewUnboundedChan[[]transfer](context.Background(), 30) // 非订单队列
var transferQueue = chanx.NewUnboundedChan[[]transfer](context.Background(), 30) // 交易转账队列

// lookbackDone 记录已触发过回溯的订单 ID，每个订单只回溯一次。
var lookbackDone sync.Map // key: int64 order ID, value: struct{}

const batchInterval = time.Second * 1       // 批处理缓解数据库读取压力
const orderCheckInterval = time.Second * 10 // 订单过期检查间隔

func init() {
	Register(Task{Callback: orderTransferHandle})
	Register(Task{Callback: notOrderTransferHandle})
	Register(Task{Callback: tronResourceHandle})
}

func orderTransferHandle(ctx context.Context) {
	var batch = make([]transfer, 0, 1000)
	var lastCheckTime = time.Now()
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case transfers, ok := <-transferQueue.Out:
			if !ok {
				return
			}
			batch = append(batch, transfers...)
		case <-ticker.C:
			// 每10秒强制检查一次过期订单，即使没有交易，防止无交易时订单不过期
			var shouldCheck = time.Since(lastCheckTime) >= orderCheckInterval
			if shouldCheck {
				lastCheckTime = time.Now()
			}

			if len(batch) == 0 {
				if shouldCheck {
					expireWaitingOrders()
				}

				continue
			}

			var other = make([]transfer, 0)
			var pending = make([]transfer, 0)
			var orders = getReceivableOrders()

			for _, t := range batch {
				// 判断数额是否在允许范围内
				if !model.IsAmountValid(t.TradeType, t.Amount) {
					continue
				}

				mqttPublish(t)

				key := fmt.Sprintf("%s%s", t.RecvAddress, t.TradeType)
				orderList, ok := orders[key]
				if !ok {
					pending = append(pending, t)
					continue
				}

				var matched bool
				for i, o := range orderList {
					if !orderTransferMatch(o, t) {
						continue
					}

					// 订单匹配 进入确认流程
					if err := o.MarkConfirming(t.BlockNum, t.FromAddress, t.TxHash, t.Timestamp, t.Amount); err != nil {
						log.Task.Warn("mark order confirming failed:", err)
						continue
					}

					// 从内存 map 中移除已匹配订单，防止同批次其他 transfer 重复匹配
					orders[key] = append(orderList[:i], orderList[i+1:]...)
					matched = true
					break
				}

				if !matched {
					pending = append(pending, t)
				}
			}

			// 容差阶段：等本批次全部精确匹配落定后再跑容差，
			// 否则先到的少付款可能抢走本批次里另一笔确切付款对应的订单
			if model.MatchMode(model.GetC(model.PaymentMatchMode)) == model.Tolerance && len(pending) > 0 {
				var rest = make([]transfer, 0, len(pending))
				for _, t := range pending {
					if !toleranceMatch(orders, t) {
						rest = append(rest, t)
					}
				}

				pending = rest
			}

			if len(pending) > 0 {
				other = append(other, pending...)
			}

			batch = batch[:0]

			if shouldCheck {
				expireWaitingOrders()
			}
		}
	}
}

func orderTransferMatch(o model.Order, t transfer) bool {
	if !orderBaseMatch(o, t) {
		return false
	}
	if !o.AddressLocked && !amountMatch(t.Amount, o.Amount, string(o.TradeType)) {
		return false
	}

	return true
}

// orderBaseMatch 只校验与金额无关的匹配条件：交易类型、收款地址、时间窗口
func orderBaseMatch(o model.Order, t transfer) bool {
	if o.TradeType != t.TradeType || orderMatchAddress(o) != t.RecvAddress {
		return false
	}
	if !o.CreatedAt.Before(t.Timestamp) || !o.ExpiredAt.After(t.Timestamp) {
		return false
	}

	return true
}

// toleranceMatch 容差阶段的兜底匹配：只处理少付场景，且候选订单必须唯一。
// 地址锁定订单已在精确阶段匹配完毕，此处忽略；多付费不在此模式处理范围内。
func toleranceMatch(orders map[string][]model.Order, t transfer) bool {
	tolerance := model.GetMatchTolerance()
	if !tolerance.GreaterThan(decimal.Zero) {
		return false
	}

	key := fmt.Sprintf("%s%s", t.RecvAddress, t.TradeType)
	orderList, ok := orders[key]
	if !ok {
		return false
	}

	candidate, gap, count := pickToleranceCandidate(orderList, t, tolerance)
	switch {
	case count > 1:
		// 多个候选说明该笔付款可能属于多笔订单，不能赌，交由人工核对补单
		log.Task.Warn(fmt.Sprintf("容差匹配存在多个候选订单，跳过人工处理：%s %s %s",
			t.TxHash, t.RecvAddress, t.Amount.String()))

		return false
	case candidate < 0:
		return false
	}

	o := orderList[candidate]
	if err := o.MarkConfirming(t.BlockNum, t.FromAddress, t.TxHash, t.Timestamp, t.Amount); err != nil {
		log.Task.Warn("mark order confirming failed:", err)

		return false
	}

	orders[key] = append(orderList[:candidate], orderList[candidate+1:]...)
	log.Task.Info(fmt.Sprintf("容差匹配成功：订单 %s 应到 %s 实到 %s，少付 %s",
		o.TradeId, o.Amount, t.Amount.String(), gap.String()))

	return true
}

// pickToleranceCandidate 在候选订单中挑出容差匹配对象。
// 返回命中的下标、少付额度、以及容差区间内的候选总数；调用方必须校验 count == 1 才能下单。
func pickToleranceCandidate(orderList []model.Order, t transfer, tolerance decimal.Decimal) (int, decimal.Decimal, int) {
	var index = -1
	var gap decimal.Decimal
	var count int

	for i, o := range orderList {
		if o.AddressLocked || !orderBaseMatch(o, t) {
			continue
		}

		target, err := decimal.NewFromString(o.Amount)
		if err != nil {
			continue
		}

		// 少付额度 = 订单金额 - 实付金额，允许区间 (0, tolerance]
		diff := target.Sub(t.Amount)
		if diff.IsNegative() || diff.IsZero() || diff.GreaterThan(tolerance) {
			continue
		}

		count++
		if index < 0 {
			index = i
			gap = diff
		}
	}

	return index, gap, count
}

func orderMatchAddress(o model.Order) string {
	if o.MatchAddress != "" {
		return o.MatchAddress
	}

	return o.Address
}

func notOrderTransferHandle(ctx context.Context) {
	var batch = make([]transfer, 0, 1000)
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case transfers, ok := <-notOrderQueue.Out:
			if !ok {
				return
			}
			batch = append(batch, transfers...)
		case <-ticker.C:
			if len(batch) == 0 {
				continue
			}

			var was = make([]model.Wallet, 0)
			model.Db.Where("other_notify = ?", model.WaOtherEnable).Find(&was)
			for _, wa := range was {
				for _, t := range batch {
					if t.RecvAddress != wa.MatchAddr && t.FromAddress != wa.MatchAddr {
						continue
					}

					if !model.IsNeedNotifyByTxid(t.TxHash) {
						continue
					}

					var record = model.NotifyRecord{Txid: t.TxHash}
					model.Db.Create(&record)

					notifier.NonOrderTransfer(model.TronTransfer(t), wa)
				}
			}

			batch = batch[:0]
		}
	}
}

func tronResourceHandle(ctx context.Context) {
	var batch = make([]resource, 0, 1000)
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case resources, ok := <-resourceQueue.Out:
			if !ok {
				return
			}
			batch = append(batch, resources...)
		case <-ticker.C:
			if len(batch) == 0 {
				continue
			}

			var was []model.Wallet
			model.Db.Where("status = ? and other_notify = ?", model.WaStatusEnable, model.WaOtherEnable).Find(&was)

			for _, wa := range was {
				if wa.GetNetwork() != conf.Tron {
					// 只有 Tron 网络目前才有资源变更通知
					continue
				}

				for _, r := range batch {
					if r.RecvAddress != wa.Address && r.FromAddress != wa.Address {
						continue
					}
					if r.ResourceCode != core.ResourceCode_ENERGY {
						continue
					}
					if !model.IsNeedNotifyByTxid(r.ID) {
						continue
					}

					var record = model.NotifyRecord{Txid: r.ID}
					model.Db.Create(&record)

					notifier.TronResourceChange(model.TronResource(r))
				}
			}

			batch = batch[:0]
		}
	}
}

func markFinalConfirmed(o model.Order) {
	o.SetSuccess()
	notifyOrderSuccess(o)
}

func receivableOrderStatuses() []int {
	return []int{model.OrderStatusWaiting, model.OrderStatusExpired, model.OrderStatusConfirming}
}

func getReceivableOrders() map[string][]model.Order {
	var orders []model.Order
	db := model.Db.Where("status in (?)", receivableOrderStatuses()).
		Where("expired_at > ?", time.Now().Add(model.GetLookbackHour())).
		Order("created_at asc")
	db.Find(&orders)

	data := make(map[string][]model.Order)
	for _, t := range orders {
		key := orderMatchAddress(t) + string(t.TradeType)
		data[key] = append(data[key], t)
	}

	return data
}

func hasLookbackOrders(tradeType []model.TradeType) bool {
	var count int64
	db := model.Db.Model(&model.Order{}).
		Where("status in (?)", receivableOrderStatuses()).
		Where("expired_at > ?", time.Now().Add(model.GetLookbackHour()))
	if len(tradeType) > 0 {
		db = db.Where("trade_type in (?)", tradeType)
	}

	db.Count(&count)

	return count > 0
}

func getLookbackUnix(network model.Network) (startAt, endAt int64, ok bool) {
	startAt, endAt, orderIDs, ok := pendingLookbackUnix(network)
	if ok {
		markLookbackDone(orderIDs)
	}

	return startAt, endAt, ok
}

func pendingLookbackUnix(network model.Network) (startAt, endAt int64, orderIDs []int64, ok bool) {
	trade := model.GetNetworkTrades(network)
	if len(trade) == 0 {
		return
	}

	lookback := time.Now().Add(model.GetLookbackHour())
	var all []model.Order
	model.Db.Model(&model.Order{}).
		Where("status in (?) and trade_type in (?)", receivableOrderStatuses(), trade).
		Where("expired_at > ?", lookback).
		Order("created_at asc").
		Find(&all)

	// 过滤掉已经回溯过的订单
	pending := make([]model.Order, 0, len(all))
	for _, o := range all {
		if _, done := lookbackDone.Load(o.ID); !done {
			pending = append(pending, o)
		}
	}
	if len(pending) == 0 {
		return
	}
	orderIDs = make([]int64, 0, len(pending))
	for _, o := range pending {
		orderIDs = append(orderIDs, o.ID)
	}

	// 起点：最早的创建时间（已按 created_at asc 排序）
	startAt = pending[0].CreatedAt.Time().Unix()

	// 终点：最晚的已过期 expired_at；若全部尚未过期则用当前时间
	endAt = time.Now().Unix()
	for _, o := range pending {
		if o.ExpiredAt.Before(time.Now()) && o.ExpiredAt.Unix() > startAt {
			endAt = o.ExpiredAt.Unix()
		}
	}

	ok = true

	return
}

func markLookbackDone(orderIDs []int64) {
	for _, orderID := range orderIDs {
		lookbackDone.Store(orderID, struct{}{})
	}
}

func expireWaitingOrders() {
	for _, t := range model.GetOrderByStatus(model.OrderStatusWaiting) {
		if time.Now().Unix() < t.ExpiredAt.Unix() {
			continue
		}

		t.SetExpired()
		notify.Bepusdt(t)
	}
}

func getConfirmingOrders(tradeType []model.TradeType) []model.Order {
	var orders = make([]model.Order, 0)
	var data = make([]model.Order, 0)
	var db = model.Db.Where("status = ?", model.OrderStatusConfirming)
	if len(tradeType) > 0 {
		db = db.Where("trade_type in (?)", tradeType)
	}

	db.Find(&orders)

	for _, order := range orders {
		if time.Now().Unix() >= order.ExpiredAt.Unix() {
			if order.ConfirmedAt == nil || order.ConfirmedAt.IsZero() || !order.ConfirmedAt.Before(order.ExpiredAt) {
				order.SetFailed()
				notify.Bepusdt(order)

				continue
			}
		}

		data = append(data, order)
	}

	return data
}

func amountMatch(amount decimal.Decimal, target, tradeType string) bool {
	mode := model.GetC(model.PaymentMatchMode)
	switch model.MatchMode(mode) {
	case model.Classic:
		return amount.String() == target
	case model.HasPrefix:
		s := amount.String()
		if !strings.HasPrefix(s, target) {
			return false
		}
		rest := s[len(target):]
		if rest == "" {
			return true
		}

		return strings.Contains(target, ".") || strings.HasPrefix(rest, ".")
	case model.RoundOff:
		t, err := decimal.NewFromString(target)
		if err != nil {
			log.Warn(err.Error())

			return false
		}

		_, precision := model.GetAtomicity(model.TradeType(tradeType)) // 标准精度
		precision2 := abs(t.Exponent())                                // 实际精度
		if precision2 != precision {
			precision = precision2
		}

		a := amount.Round(precision)
		t = t.Round(precision)

		return a.Equal(t)
	case model.Tolerance:
		// 容差模式的第一阶段仍是精确匹配，使用数值相等而非字符串相等，
		// 这样 10.1 与 10.10 视为一致；少付的兜底匹配在 toleranceMatch 中进行。
		t, err := decimal.NewFromString(target)
		if err != nil {
			log.Warn(err.Error())

			return false
		}

		return amount.Equal(t)
	}

	return false
}

func abs(n int32) int32 {
	if n < 0 {
		return -n
	}
	return n
}
