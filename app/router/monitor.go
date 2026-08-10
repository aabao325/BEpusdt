package router

import (
	"github.com/gin-gonic/gin"
	"github.com/v03413/bepusdt/app/handler/monitor"
)

// monitorInit 注册外部监控接口。
//
// 注意：此处刻意不使用 GetRegister，因其会将路由登记进 authRoute / secureRoute，
// 从而要求 24 小时内存会话令牌与安全入口 Cookie，不适用于长期无人值守的监控轮询。
// 该接口自行校验数据库持久化的监控令牌。
func monitorInit(e *gin.Engine) {
	var mRtr = e.Group("/api/monitor")
	var mHdr = new(monitor.Monitor)
	{
		mRtr.GET("/stats", mHdr.Stats)
	}
}
