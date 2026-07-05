package service

import (
	"context"
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestTradeFee 佣金最低 5 元 + A 股股票卖出印花税；ETF/基金免印花税。
func TestTradeFee(t *testing.T) {
	// 小额买入走最低佣金 5，无税。
	fee, tax := tradeFee("cn", model.PaperSideBuy, "600000", 1000)
	if fee != 5 || tax != 0 {
		t.Fatalf("小额买入应佣金5无税，得到 fee=%v tax=%v", fee, tax)
	}
	// 大额卖出：佣金万2.5 + 印花税万5。
	fee, tax = tradeFee("cn", model.PaperSideSell, "600000", 100000)
	if fee != 25 || tax != 50 {
		t.Fatalf("大额卖出 fee 应25 tax应50，得到 fee=%v tax=%v", fee, tax)
	}
	// ETF/场内基金卖出免印花税（佣金照收）。
	fee, tax = tradeFee("cn", model.PaperSideSell, "510300", 100000)
	if fee != 25 || tax != 0 {
		t.Fatalf("沪 ETF 卖出应佣金25无税，得到 fee=%v tax=%v", fee, tax)
	}
	if _, tax = tradeFee("cn", model.PaperSideSell, "159915", 100000); tax != 0 {
		t.Fatalf("深 ETF 卖出不应有印花税，得到 %v", tax)
	}
	// 美股卖出无印花税。
	_, tax = tradeFee("us", model.PaperSideSell, "AAPL", 100000)
	if tax != 0 {
		t.Fatalf("美股卖出不应有印花税，得到 %v", tax)
	}
}

// TestIsCNFund 场内基金前缀判定：沪 50/51/56/58、深 15/16/18 为基金；个股/可转债不是。
func TestIsCNFund(t *testing.T) {
	funds := []string{"510300", "512880", "588000", "159915", "159949", "160106", "184688", "501018", "508056"}
	for _, s := range funds {
		if !isCNFund(s) {
			t.Errorf("%s 应判为场内基金", s)
		}
	}
	notFunds := []string{"600000", "000001", "300750", "688981", "110038", "113050", "51030"}
	for _, s := range notFunds {
		if isCNFund(s) {
			t.Errorf("%s 不应判为场内基金", s)
		}
	}
}

// TestPaperTradeFlow 买入→加仓（均价）→卖出（已实现盈亏 + 现金）全流程 + 不足校验 + 重置。
// 显式传 Price>0，Trade 跳过行情取数，可离线测（market 的 mgr 不被触达）。
func TestPaperTradeFlow(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM paper_accounts")
	common.DB.Exec("DELETE FROM paper_holdings")
	common.DB.Exec("DELETE FROM paper_trades")
	svc := &PaperService{market: &MarketService{}}
	ctx := context.Background()
	const uid = 1

	// 买入 1000 股 @10 → amount=10000 fee=5 costBasis=10005，现金 100000-10005=89995。
	if _, err := svc.Trade(ctx, uid, TradeInput{Symbol: "600000", Market: "cn", Side: "buy", Price: 10, Quantity: 1000}); err != nil {
		t.Fatalf("买入失败: %v", err)
	}
	acc, _ := svc.GetOrCreateAccount(uid)
	if acc.Cash != 89995 {
		t.Fatalf("买入后现金应 89995，得到 %v", acc.Cash)
	}

	// 加仓 1000 股 @12 → amount=12000 fee=5 costBasis=12005；均价=(10005+12005)/2000=11.005。
	if _, err := svc.Trade(ctx, uid, TradeInput{Symbol: "600000", Market: "cn", Side: "buy", Price: 12, Quantity: 1000}); err != nil {
		t.Fatalf("加仓失败: %v", err)
	}
	var h model.PaperHolding
	common.DB.Where("user_id = ? AND symbol = ?", uid, "600000").First(&h)
	if h.Quantity != 2000 || h.AvgCost != 11.005 {
		t.Fatalf("加仓后应 2000 股均价 11.005，得到 %v @ %v", h.Quantity, h.AvgCost)
	}

	// 现金不足：买入超大单应报错。
	if _, err := svc.Trade(ctx, uid, TradeInput{Symbol: "000001", Market: "cn", Side: "buy", Price: 1000, Quantity: 100000}); err == nil {
		t.Fatalf("现金不足应报错")
	}
	// 持仓不足：卖出超过持有量应报错。
	if _, err := svc.Trade(ctx, uid, TradeInput{Symbol: "600000", Market: "cn", Side: "sell", Price: 12, Quantity: 5000}); err == nil {
		t.Fatalf("持仓不足应报错")
	}

	// 卖出 2000 股 @13 → amount=26000 fee=6.5 tax=13 proceeds=25980.5；
	// realized = 25980.5 - 11.005*2000 = 25980.5 - 22010 = 3970.5。
	tr, err := svc.Trade(ctx, uid, TradeInput{Symbol: "600000", Market: "cn", Side: "sell", Price: 13, Quantity: 2000})
	if err != nil {
		t.Fatalf("卖出失败: %v", err)
	}
	if tr.RealizedPnl != 3970.5 {
		t.Fatalf("已实现盈亏应 3970.5，得到 %v", tr.RealizedPnl)
	}
	// 清仓后持仓应删除。
	var cnt int64
	common.DB.Model(&model.PaperHolding{}).Where("user_id = ?", uid).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("清仓后不应有持仓，剩 %d", cnt)
	}

	// Overview：累计已实现盈亏。
	ov, err := svc.Overview(ctx, uid)
	if err != nil {
		t.Fatalf("Overview 失败: %v", err)
	}
	if ov.RealizedPnl != 3970.5 {
		t.Fatalf("累计已实现盈亏应 3970.5，得到 %v", ov.RealizedPnl)
	}

	// 用户隔离：用户2 是全新账户。
	ov2, _ := svc.Overview(ctx, 2)
	if ov2.Account.Cash != model.PaperDefaultCash || len(ov2.Holdings) != 0 {
		t.Fatalf("用户2 应为默认新账户")
	}

	// ETF 三位小数限价保真（最小变动价位 0.001，round2 会抹掉第三位）。
	etfTr, err := svc.Trade(ctx, 2, TradeInput{Symbol: "159915", Market: "cn", Side: "buy", Price: 2.345, Quantity: 100})
	if err != nil {
		t.Fatalf("ETF 买入失败: %v", err)
	}
	if etfTr.Price != 2.345 {
		t.Fatalf("ETF 成交价应保留三位小数 2.345，得到 %v", etfTr.Price)
	}
	if etfTr.Tax != 0 {
		t.Fatalf("ETF 买入不应有印花税，得到 %v", etfTr.Tax)
	}

	// 重置。
	if _, err := svc.Reset(uid, 50000); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	acc, _ = svc.GetOrCreateAccount(uid)
	if acc.Cash != 50000 || acc.InitialCash != 50000 {
		t.Fatalf("重置后现金/初始应 50000，得到 %v/%v", acc.Cash, acc.InitialCash)
	}
	common.DB.Model(&model.PaperTrade{}).Where("user_id = ?", uid).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("重置应清空流水，剩 %d", cnt)
	}
}
