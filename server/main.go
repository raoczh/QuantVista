package main

import (
	"embed"
	"os"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/middleware"
	"quantvista/model"
	"quantvista/router"
	"quantvista/service"
	"quantvista/setting"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

//go:embed all:web/dist
var webFS embed.FS

func main() {
	// 本地开发：尝试加载 .env（生产由容器环境变量注入，无文件不报错）。
	for _, p := range []string{".env", "../deploy/.env"} {
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p)
			break
		}
	}

	common.InitConfig()
	common.SysLog("QuantVista %s 启动中 ...", common.Version)

	if err := common.InitDB(); err != nil {
		common.FatalLog("数据库初始化失败: %v", err)
	}
	if err := model.Migrate(); err != nil {
		common.FatalLog("数据库迁移失败: %v", err)
	}
	if err := setting.Init(); err != nil {
		common.FatalLog("系统设置初始化失败: %v", err)
	}
	service.StartRefreshTokenJanitor()
	if err := common.InitRedis(); err != nil {
		// Redis 是可选项，失败仅告警不致命。
		common.SysWarn("Redis 初始化失败（缓存关闭）: %v", err)
	}

	mgr := datasource.DefaultManager()
	service.StartMarketJobs(mgr)
	service.StartTrackingJobs(mgr)
	service.StartAlertJobs(mgr)

	if !common.DebugEnabled {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	// 反代（宝塔/nginx）场景通过 TRUSTED_PROXIES 显式声明可信代理；
	// 默认不信任任何代理头，避免伪造 X-Forwarded-For 绕过按 IP 的限流。
	if err := engine.SetTrustedProxies(common.TrustedProxies); err != nil {
		common.FatalLog("TRUSTED_PROXIES 配置无效: %v", err)
	}
	engine.Use(middleware.Recovery(), middleware.Logger(), middleware.CORS())

	router.SetApiRouter(engine, mgr)
	router.SetWebRouter(engine, webFS)

	addr := ":" + common.Port
	common.SysLog("监听 %s", addr)
	if err := engine.Run(addr); err != nil {
		common.FatalLog("服务启动失败: %v", err)
	}
}
