package task

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/v03413/bepusdt/app/model"
)

// newToleranceTestOrder CreatedAt 来自嵌入的 AutoTimeAt，结构体字面量里无法直接赋值
func newToleranceTestOrder(amount, address string, createdAt time.Time) model.Order {
	var order = model.Order{
		TradeId:      "T" + amount,
		TradeType:    model.UsdtBep20,
		Amount:       amount,
		Address:      address,
		MatchAddress: address,
		ExpiredAt:    createdAt.Add(20 * time.Minute),
	}
	order.CreatedAt = (*model.Datetime)(&createdAt)

	return order
}

func newToleranceTestTransfer(amount, address string, at time.Time) transfer {
	return transfer{
		TradeType:   model.UsdtBep20,
		Amount:      decimal.RequireFromString(amount),
		RecvAddress: address,
		Timestamp:   at,
	}
}

func TestPickToleranceCandidate(t *testing.T) {
	var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.Local)
	var constAddr = "0xabc"
	var tolerance = decimal.RequireFromString("0.01")

	tests := []struct {
		name       string
		orders     []model.Order
		paid       string
		wantIndex  int
		wantCount  int
		wantGap    string
		wantPayOff string
	}{
		{
			name:      "少付刚好等于容差",
			orders:    []model.Order{newToleranceTestOrder("10.01", constAddr, base.Add(-time.Minute))},
			paid:      "10.00",
			wantIndex: 0,
			wantCount: 1,
			wantGap:   "0.01",
		},
		{
			name:      "少付超出容差",
			orders:    []model.Order{newToleranceTestOrder("10.02", constAddr, base.Add(-time.Minute))},
			paid:      "10.00",
			wantIndex: -1,
			wantCount: 0,
		},
		{
			name:      "足额与多付都不属于容差范围",
			orders:    []model.Order{newToleranceTestOrder("10.00", constAddr, base.Add(-time.Minute))},
			paid:      "10.00",
			wantIndex: -1,
			wantCount: 0,
		},
		{
			name:      "多付不走容差",
			orders:    []model.Order{newToleranceTestOrder("9.99", constAddr, base.Add(-time.Minute))},
			paid:      "10.00",
			wantIndex: -1,
			wantCount: 0,
		},
		{
			name: "多个候选必须全部放弃",
			orders: []model.Order{
				newToleranceTestOrder("10.01", constAddr, base.Add(-2*time.Minute)),
				newToleranceTestOrder("10.005", constAddr, base.Add(-time.Minute)),
			},
			paid:      "10.00",
			wantIndex: 0,
			wantCount: 2,
			wantGap:   "0.01",
		},
		{
			name:      "地址锁定订单不参与容差",
			orders: []model.Order{func() model.Order {
				var order = model.Order{TradeType: model.UsdtBep20, Amount: "10.01", Address: constAddr, MatchAddress: constAddr, AddressLocked: true, ExpiredAt: base.Add(10 * time.Minute)}
				order.CreatedAt = (*model.Datetime)(ptrTime(base.Add(-time.Minute)))

				return order
			}()},
			paid:      "10.00",
			wantIndex: -1,
			wantCount: 0,
		},
		{
			name:      "交易时间早于订单创建时间不匹配",
			orders:    []model.Order{newToleranceTestOrder("10.01", constAddr, base.Add(time.Minute))},
			paid:      "10.00",
			wantIndex: -1,
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var at = base
			if tc.name == "交易时间早于订单创建时间不匹配" {
				at = base.Add(-time.Minute)
			}

			index, gap, count := pickToleranceCandidate(tc.orders, newToleranceTestTransfer(tc.paid, constAddr, at), tolerance)
			if index != tc.wantIndex {
				t.Fatalf("index = %d, want %d", index, tc.wantIndex)
			}
			if count != tc.wantCount {
				t.Fatalf("count = %d, want %d", count, tc.wantCount)
			}
			if tc.wantGap != "" && !gap.Equal(decimal.RequireFromString(tc.wantGap)) {
				t.Fatalf("gap = %s, want %s", gap, tc.wantGap)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
