package cmd

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/v03413/bepusdt/app/model"
)

var Monitor = &cli.Command{
	Name:  "monitor",
	Usage: "查看或重置节点状态监控令牌，用于外部监控系统轮询节点成功率",
	Flags: []cli.Flag{
		SQLiteFlag,
		PostgresDSNFlag,
		&cli.BoolFlag{
			Name:  "reset",
			Usage: "重置监控令牌，旧令牌立即失效",
		},
	},
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if err := model.Init(c.String("sqlite"), c.String("postgres")); err != nil {

			return ctx, fmt.Errorf("数据库初始化失败 %w", err)
		}

		return ctx, nil
	},
	After: func(ctx context.Context, c *cli.Command) error {
		model.Close()

		return nil
	},
	Action: func(ctx context.Context, cmd *cli.Command) error {
		var token string
		if cmd.Bool("reset") {
			token = model.ResetMonitorToken()
			fmt.Println("监控令牌已重置，旧令牌立即失效。")
		} else {
			token = model.MonitorToken()
		}

		fmt.Println()
		fmt.Println("┏━━  📡  节点状态监控")
		fmt.Println("┃")
		fmt.Printf("┃    🎫  监控令牌:  %s\n", token)
		fmt.Println("┃")
		fmt.Println("┃    接口地址（GET，替换为你的实际域名）:")
		fmt.Printf("┃    https://your-domain/api/monitor/stats?token=%s\n", token)
		fmt.Println("┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println()
		fmt.Println("说明：")
		fmt.Println("    •  该令牌持久保存于数据库，不随后台会话过期，进程重启后依然有效")
		fmt.Println("    •  令牌与后台登录令牌相互独立，后台登录、改密、退出均不会使其失效")
		fmt.Println("    •  接口只读，不返回任何 RPC 端点与 API Key")
		fmt.Println("    •  可选参数：threshold 成功率阈值(默认50)、stale 陈旧秒数阈值(默认300)、window=recent|total")
		fmt.Println("    •  泄露后使用 monitor --reset 重置")
		fmt.Println()

		return nil
	},
}
