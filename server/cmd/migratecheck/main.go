// migratecheck 是一次性迁移验证工具：连接 SQL_DSN 指定的数据库并执行与服务
// 启动完全相同的自动迁移，用于在真实 MySQL 上复现/验证升级路径。不启动服务。
package main

import (
	"fmt"
	"os"

	"quantvista/common"
	"quantvista/model"
)

func main() {
	if err := common.InitDB(); err != nil {
		fmt.Println("INITDB ERROR:", err)
		os.Exit(1)
	}
	if err := model.Migrate(); err != nil {
		fmt.Println("MIGRATE ERROR:", err)
		os.Exit(1)
	}
	fmt.Println("MIGRATE OK")
}
