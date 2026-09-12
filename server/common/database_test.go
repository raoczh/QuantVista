package common

import (
	"testing"

	mysqlconfig "github.com/go-sql-driver/mysql"
)

func TestMySQLDSNAlwaysParsesTimes(t *testing.T) {
	for _, dsn := range []string{
		"review:has-parseTime-in-password@tcp(127.0.0.1:33317)/review",
		"review:password@tcp(127.0.0.1:33317)/parseTime_database",
		"review:password@tcp(127.0.0.1:33317)/review?parseTime=false&loc=Asia%2FShanghai&charset=utf8mb4",
	} {
		before, err := mysqlconfig.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := mysqlDSNWithTime(dsn)
		if err != nil {
			t.Fatal(err)
		}
		after, err := mysqlconfig.ParseDSN(normalized)
		if err != nil || !after.ParseTime {
			t.Fatalf("必须启用日期解析：err=%v", err)
		}
		if before.User != after.User || before.Passwd != after.Passwd || before.Addr != after.Addr ||
			before.DBName != after.DBName || before.Loc.String() != after.Loc.String() || before.Params["charset"] != after.Params["charset"] {
			t.Fatal("标准化时间解析选项不得改动凭证、目标库、时区或字符集")
		}
	}
}
