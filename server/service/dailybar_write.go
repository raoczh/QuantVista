package service

import (
	"context"
	"errors"
	"sort"
	"sync"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 本进程重入快速返回；数据库行锁仍负责不同进程之间的完整事务隔离。
var dailyBarWrites sync.Map

// withDailyBarWriteTx 在任何普通日线读取前持有标的锁。MySQL 可重复读的读视图
// 因而在等待锁之后建立，校验与写入不能跨越另一个重锚事务。
func withDailyBarWriteTx(ctx context.Context, market string, symbols []string, fn func(*gorm.DB) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	ordered := append([]string(nil), symbols...)
	sort.Strings(ordered)
	locked := make([]model.DailyBarWriteLock, 0, len(ordered))
	defer func() {
		for _, row := range locked {
			dailyBarWrites.Delete(QuoteKey(row.Market, row.Symbol))
		}
	}()
	for _, symbol := range ordered {
		if len(locked) > 0 && locked[len(locked)-1].Symbol == symbol {
			continue
		}
		if _, busy := dailyBarWrites.LoadOrStore(QuoteKey(market, symbol), struct{}{}); busy {
			return errRebaseInProgress
		}
		locked = append(locked, model.DailyBarWriteLock{Market: market, Symbol: symbol})
	}
	if len(locked) == 0 {
		return nil
	}
	return common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// INSERT/锁定读都不会在 MySQL 提前建立可重复读快照。
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(locked, 500).Error; err != nil {
			return err
		}
		var rows []model.DailyBarWriteLock
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("market = ? AND symbol IN ?", market, ordered).
			Order("symbol").Find(&rows).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

// persistWideBarBatch 对批量增量在提交锁内再次核对昨收锚点。初筛到提交之间
// 可能已有在线请求重锚，不能把初筛结论当作后续写入的永久授权。
func persistWideBarBatch(ctx context.Context, spots []datasource.SpotRow, tradeDate string) (int, []string, error) {
	symbols := make([]string, 0, len(spots))
	for _, spot := range spots {
		symbols = append(symbols, spot.Symbol)
	}
	blocked := []string{}
	written := 0
	err := withDailyBarWriteTx(ctx, "cn", symbols, func(tx *gorm.DB) error {
		latest := tx.Model(&model.DailyBar{}).Select("symbol, MAX(trade_date) AS trade_date").
			Where("market = ? AND symbol IN ? AND trade_date < ?", "cn", symbols, tradeDate).Group("symbol")
		var anchors []model.DailyBar
		if err := tx.Table("daily_bars AS b").Select("b.symbol, b.close").
			Joins("JOIN (?) prior ON prior.symbol = b.symbol AND prior.trade_date = b.trade_date", latest).
			Where("b.market = ?", "cn").Scan(&anchors).Error; err != nil {
			return err
		}
		priorClose := map[string]float64{}
		for _, anchor := range anchors {
			priorClose[anchor.Symbol] = anchor.Close
		}
		var mixed []string
		if err := tx.Model(&model.DailyBar{}).Where("market = ? AND symbol IN ? AND source = ?", "cn", symbols, "sina").
			Distinct("symbol").Pluck("symbol", &mixed).Error; err != nil {
			return err
		}
		unadjusted := map[string]bool{}
		for _, symbol := range mixed {
			unadjusted[symbol] = true
		}
		bars := make([]model.DailyBar, 0, len(spots))
		accepted := make([]string, 0, len(spots))
		for _, spot := range spots {
			prior, exists := priorClose[spot.Symbol]
			if unadjusted[spot.Symbol] || (exists && (prior <= 0 || spot.PrevClose <= 0 || relDiff(prior, spot.PrevClose) > rebaseTolerance)) {
				blocked = append(blocked, spot.Symbol)
				continue
			}
			bars = append(bars, model.DailyBar{Market: "cn", Symbol: spot.Symbol, TradeDate: tradeDate,
				Open: spot.Open, High: spot.High, Low: spot.Low, Close: spot.Price,
				Volume: spot.Volume, Amount: spot.Amount, TurnoverRate: spot.TurnoverRate, Source: "eastmoney"})
			accepted = append(accepted, spot.Symbol)
		}
		if len(blocked) > 0 {
			if err := tx.Model(&model.MarketSyncState{}).Where("market = ? AND symbol IN ?", "cn", blocked).
				Updates(map[string]any{"init_status": "pending", "fail_count": 0}).Error; err != nil {
				return err
			}
		}
		if len(bars) == 0 {
			return nil
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"open", "high", "low", "close", "volume", "amount", "turnover_rate", "source"}),
		}).CreateInBatches(bars, 500).Error; err != nil {
			return err
		}
		// 仅为实际已提交的 bar 推进水位，且不能回退另一路同步已写入的更新日期。
		if err := tx.Model(&model.MarketSyncState{}).Where("market = ? AND symbol IN ? AND COALESCE(last_bar_date, '') < ?", "cn", accepted, tradeDate).
			Update("last_bar_date", tradeDate).Error; err != nil {
			return err
		}
		written = len(bars)
		return nil
	})
	if errors.Is(err, errRebaseInProgress) {
		if len(spots) == 1 {
			return 0, symbols, nil // 同一标的在另一路提交；不消耗初始化失败次数。
		}
		// 只隔离有争用的标的，其余股票仍批量落库，避免一次在线请求拖垮全轮同步。
		mid := len(spots) / 2
		n1, b1, err := persistWideBarBatch(ctx, spots[:mid], tradeDate)
		if err != nil {
			return n1, b1, err
		}
		n2, b2, err := persistWideBarBatch(ctx, spots[mid:], tradeDate)
		return n1 + n2, append(b1, b2...), err
	}
	if err != nil {
		return 0, nil, err // 本批事务已回滚。
	}
	return written, blocked, nil
}
