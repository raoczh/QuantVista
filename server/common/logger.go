package common

import (
	"fmt"
	"log"
	"os"
	"time"
)

// 极简结构化日志。骨架阶段先用标准 log，后续可替换为 zap/slog。
var stdLogger = log.New(os.Stdout, "", 0)

func logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	stdLogger.Printf("[%s] %s | %s", level, time.Now().Format("2006-01-02 15:04:05"), msg)
}

func SysLog(format string, args ...any)   { logf("INFO", format, args...) }
func SysWarn(format string, args ...any)  { logf("WARN", format, args...) }
func SysError(format string, args ...any) { logf("ERROR", format, args...) }

// FatalLog 打印后退出，仅用于启动期不可恢复错误。
func FatalLog(format string, args ...any) {
	logf("FATAL", format, args...)
	os.Exit(1)
}
