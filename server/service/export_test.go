package service

import (
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestExportPositionsCSV 导出：BOM 头 + 表头 + 用户隔离（DB 集成）。
func TestExportPositionsCSV(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM positions")
	svc := NewExportService()

	common.DB.Create(&model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		PositionType: model.PositionTypeShortTerm, Status: model.PositionStatusHolding,
		BuyPrice: 8.5, BuyDate: "2026-06-01", Quantity: 1000, BuyReason: "测试, 含逗号"})
	common.DB.Create(&model.Position{UserID: 2, Symbol: "000001", Market: "cn",
		PositionType: model.PositionTypeLongTerm, Status: model.PositionStatusHolding,
		BuyPrice: 10, BuyDate: "2026-06-02", Quantity: 100})

	data, filename, err := svc.Export(1, "positions")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.HasPrefix(string(data), "\xEF\xBB\xBF") {
		t.Fatalf("应带 UTF-8 BOM 头")
	}
	if !strings.HasPrefix(filename, "positions-") || !strings.HasSuffix(filename, ".csv") {
		t.Fatalf("文件名格式错误: %s", filename)
	}
	body := string(data)
	if !strings.Contains(body, "600000") || strings.Contains(body, "000001") {
		t.Fatalf("应只含本人数据（用户隔离）")
	}
	// 含逗号的字段应被 CSV 引号包裹。
	if !strings.Contains(body, `"测试, 含逗号"`) {
		t.Fatalf("含逗号字段应加引号: %s", body)
	}
	// 行数 = 表头 + 1 条数据。
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("应为表头+1 行数据，得到 %d 行", len(lines))
	}

	// 非法类型。
	if _, _, err := svc.Export(1, "bogus"); err == nil {
		t.Fatalf("非法导出类型应报错")
	}
}

// TestSanitizeCSVCell 公式注入防护：危险前缀文本加单引号，合法数值/普通文本放行。
func TestSanitizeCSVCell(t *testing.T) {
	cases := []struct{ in, want string }{
		{"=SUM(A1:A2)", "'=SUM(A1:A2)"},
		{"+cmd", "'+cmd"},
		{"-cmd|calc", "'-cmd|calc"},
		{"@import", "'@import"},
		{"\tTAB", "'\tTAB"},
		{"\rCR", "'\rCR"},
		{"-4.2", "-4.2"},   // 合法负数：不污染数字列
		{"+8.5", "+8.5"},   // 合法正号数值
		{"1.5e3", "1.5e3"}, // 科学计数
		{"8.50", "8.50"},
		{"浦发银行", "浦发银行"},
		{"normal reason", "normal reason"},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizeCSVCell(c.in); got != c.want {
			t.Fatalf("sanitizeCSVCell(%q)=%q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestExportCSVFormulaInjection 端到端：以危险字符开头的 buy_reason 导出后被加固为纯文本。
func TestExportCSVFormulaInjection(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM positions")
	svc := NewExportService()
	common.DB.Create(&model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		PositionType: model.PositionTypeShortTerm, Status: model.PositionStatusHolding,
		BuyPrice: 8.5, BuyDate: "2026-06-01", Quantity: 1000,
		BuyReason: "=HYPERLINK(\"http://evil\")"})

	data, _, err := svc.Export(1, "positions")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	body := string(data)
	if strings.Contains(body, ",=HYPERLINK") {
		t.Fatalf("危险公式字段未被加固: %s", body)
	}
	if !strings.Contains(body, "'=HYPERLINK") {
		t.Fatalf("危险公式字段应被前置单引号: %s", body)
	}
}

func TestImportPositions(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM positions")
	svc := NewExportService()

	csvText := "symbol,market,type,buy_price,buy_date,quantity,buy_fee,buy_tax,reason\n" +
		"600000,cn,short_term,8.50,2026-06-01,1000,5,0,动量突破\n" + // 合法
		"600519,,长线,1700,2026-06-02,100,,,\n" + // 合法：market 默认 cn、中文类型、费税留空
		"000001,cn,short_term,-1,2026-06-01,100,,,\n" + // 坏行：价格为负
		"000002,cn,short_term,8,2026/06/01,100,,,\n" + // 坏行：日期格式
		"000004,cn,day_trade,8,2026-06-01,100,,,\n" // 坏行：类型非法

	res, err := svc.ImportPositions(1, strings.NewReader(csvText))
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if res.Imported != 2 {
		t.Fatalf("应导入 2 条，得到 %d", res.Imported)
	}
	if len(res.Failed) != 3 {
		t.Fatalf("应报告 3 条坏行，得到 %d: %+v", len(res.Failed), res.Failed)
	}
	// 坏行行号（物理行号：数据从第 2 行起）。
	if res.Failed[0].Row != 4 || res.Failed[1].Row != 5 || res.Failed[2].Row != 6 {
		t.Fatalf("坏行行号错误: %+v", res.Failed)
	}

	var ps []model.Position
	common.DB.Where("user_id = ?", 1).Order("id ASC").Find(&ps)
	if len(ps) != 2 {
		t.Fatalf("库中应有 2 条，得到 %d", len(ps))
	}
	if ps[0].Symbol != "600000" || ps[0].PositionType != model.PositionTypeShortTerm || ps[0].BuyFee != 5 {
		t.Fatalf("第一条字段错误: %+v", ps[0])
	}
	if ps[1].Symbol != "600519" || ps[1].Market != "cn" || ps[1].PositionType != model.PositionTypeLongTerm {
		t.Fatalf("第二条（默认市场/中文类型）字段错误: %+v", ps[1])
	}
	if ps[1].Status != model.PositionStatusHolding || ps[1].Name != "600519" {
		t.Fatalf("导入应为持仓中且 name 记 symbol: %+v", ps[1])
	}

	// 缺必需列拒绝。
	if _, err := svc.ImportPositions(1, strings.NewReader("symbol,market\n600000,cn\n")); err == nil {
		t.Fatalf("缺必需列应拒绝")
	}

	// BOM 表头容忍。
	res2, err := svc.ImportPositions(1, strings.NewReader("\xEF\xBB\xBFsymbol,buy_price,buy_date,quantity\n600036,9.9,2026-06-03,200\n"))
	if err != nil || res2.Imported != 1 {
		t.Fatalf("BOM 表头应容忍: %v %+v", err, res2)
	}

	// 超上限整体拒绝（不部分入库）。
	var b strings.Builder
	b.WriteString("symbol,buy_price,buy_date,quantity\n")
	for i := 0; i < importMaxRows+1; i++ {
		b.WriteString("600000,8,2026-06-01,100\n")
	}
	var before int64
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&before)
	if _, err := svc.ImportPositions(1, strings.NewReader(b.String())); err == nil {
		t.Fatalf("超上限应整体拒绝")
	}
	var after int64
	common.DB.Model(&model.Position{}).Where("user_id = ?", 1).Count(&after)
	if before != after {
		t.Fatalf("超上限拒绝时不应部分入库: %d → %d", before, after)
	}
}
