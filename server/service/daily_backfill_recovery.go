package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

const (
	DailyBackfillScopeTracked    = "tracked"
	DailyBackfillScopeMarketwide = "marketwide"
	dailyBackfillMaxOpenDays     = 250
	dailyBackfillReadBatchSize   = 200
)

func normalizeDailyBackfillScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return DailyBackfillScopeTracked, nil
	}
	if scope != DailyBackfillScopeTracked && scope != DailyBackfillScopeMarketwide {
		return "", errors.New("日线补采 scope 仅支持 tracked 或 marketwide")
	}
	return scope, nil
}

func dailyBackfillCompletedCutoff(now time.Time) time.Time {
	now = now.In(datasource.ChinaMarketLocation())
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if now.Hour()*60+now.Minute() < sessionCloseMin+5 {
		cutoff = cutoff.AddDate(0, 0, -1)
	}
	return cutoff
}

// Defaults must establish the entire tail through the last completed day, not
// silently stop at an old MAX(calendar.trade_date). Explicit windows stay local.
func dailyBackfillOpenDatesAtDB(database *gorm.DB, request MaintenanceRequest, now time.Time) (string, []string, string, string, error) {
	if database == nil {
		return "", nil, "", "", errors.New("数据库不可用")
	}
	market, err := normalizeMaintenanceMarket(request.Market)
	if err != nil {
		return "", nil, "", "", err
	}
	scope, err := normalizeDailyBackfillScope(request.Scope)
	if err != nil {
		return "", nil, "", "", err
	}
	defaultDays, maxOpenDays, maxNaturalDays := maintenanceDefaultOpenDays, maintenanceMaxOpenDays, maintenanceMaxNaturalDays
	if scope == DailyBackfillScopeMarketwide {
		defaultDays, maxOpenDays, maxNaturalDays = dailyBackfillMaxOpenDays, dailyBackfillMaxOpenDays, maintenanceMaxLookbackDays
	}
	from, to := strings.TrimSpace(request.From), strings.TrimSpace(request.To)
	if (from == "") != (to == "") {
		return "", nil, "", "", errors.New("from 与 to 必须同时提供")
	}
	defaultWindow := from == ""
	var dates []string
	if defaultWindow {
		cutoff := dailyBackfillCompletedCutoff(now)
		to = cutoff.Format(time.DateOnly)
		if err := database.Model(&model.TradingCalendar{}).
			Where("market = ? AND is_open = ? AND trade_date >= ? AND trade_date <= ?", market, true, cutoff.AddDate(0, 0, -(maxNaturalDays-1)).Format(time.DateOnly), to).
			Order("trade_date DESC").Limit(defaultDays).Pluck("trade_date", &dates).Error; err != nil {
			return "", nil, "", "", err
		}
		if len(dates) == 0 {
			return "", nil, "", "", errors.New("本地交易日历无近期已完成开市日，请先回填日历")
		}
		reverseStrings(dates)
		from = dates[0]
	}
	first, firstErr := time.ParseInLocation(time.DateOnly, from, datasource.ChinaMarketLocation())
	last, lastErr := time.ParseInLocation(time.DateOnly, to, datasource.ChinaMarketLocation())
	if firstErr != nil || lastErr != nil {
		return "", nil, "", "", errors.New("日期必须为 YYYY-MM-DD")
	}
	now = now.In(datasource.ChinaMarketLocation())
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if last.Before(first) || last.After(today) || first.Before(today.AddDate(0, 0, -maintenanceMaxLookbackDays)) {
		return "", nil, "", "", errors.New("日期范围无效：from 不得晚于 to，to 不得晚于今天，最多回看 400 个自然日")
	}
	naturalDays := int(last.Sub(first).Hours()/24) + 1
	if naturalDays > maxNaturalDays {
		return "", nil, "", "", fmt.Errorf("范围最多 %d 个自然日", maxNaturalDays)
	}
	var calendar []model.TradingCalendar
	if err := database.Select("trade_date", "is_open").Where("market = ? AND trade_date >= ? AND trade_date <= ?", market, from, to).
		Order("trade_date").Limit(maxNaturalDays + 1).Find(&calendar).Error; err != nil {
		return "", nil, "", "", err
	}
	if len(calendar) > naturalDays {
		return "", nil, "", "", errors.New("交易日历范围超过查询硬上限")
	}
	known := make(map[string]bool, len(calendar))
	dates = nil
	for _, row := range calendar {
		known[row.TradeDate] = true
		if row.IsOpen {
			dates = append(dates, row.TradeDate)
		}
	}
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday && !known[day.Format(time.DateOnly)] {
			return "", nil, "", "", fmt.Errorf("交易日历缺少 %s 的开休市事实（默认窗口须覆盖最近已完成日），请先回填同一范围日历", day.Format(time.DateOnly))
		}
	}
	if len(dates) > maxOpenDays {
		return "", nil, "", "", fmt.Errorf("范围最多 %d 个交易日", maxOpenDays)
	}
	if defaultWindow && scope == DailyBackfillScopeMarketwide && len(dates) < defaultDays {
		return "", nil, "", "", fmt.Errorf("全市场默认窗口需要 %d 个已确认交易日，本地仅有 %d；请分段回填近 400 自然日日历，或明确选择较短补采范围", defaultDays, len(dates))
	}
	return market, dates, from, to, nil
}

func dailyBackfillUniverseDB(database *gorm.DB, market, scope string) ([]model.Stock, error) {
	var stocks []model.Stock
	query := database.Model(&model.Stock{}).Select("id", "symbol", "market").Where("market = ?", market).Order("id")
	if scope == DailyBackfillScopeMarketwide {
		// UNION (not UNION ALL) deduplicates stocks that also belong to the known
		// market universe. A dry run never discovers symbols from the network.
		query = database.Table("(SELECT symbol, market FROM market_sync_states WHERE market = ? UNION SELECT symbol, market FROM stocks WHERE market = ?) AS universe", market, market).
			Select("symbol", "market").Order("symbol")
	}
	if err := query.Limit(maintenanceMaxUniverseRows + 1).Find(&stocks).Error; err != nil {
		return nil, err
	}
	if len(stocks) > maintenanceMaxUniverseRows {
		return nil, errors.New("日线补采宇宙超过 7000 只硬上限，未执行任何上游扫描")
	}
	return stocks, nil
}

// IPO dates are optional local evidence. Neither CreatedAt nor first_seen is a
// listing date. Conflicting/invalid IPO records provide no exclusion authority.
func dailyBackfillListingDatesDB(database *gorm.DB, now time.Time) (map[string]string, error) {
	listingDates := make(map[string]string)
	if !database.Migrator().HasTable(&model.IpoSubscription{}) {
		return listingDates, nil
	}
	var rows []model.IpoSubscription
	if err := database.Select("code", "apply_date", "list_date").Where("kind = ? AND list_date <> '' AND list_date <= ?", model.IpoKindStock, now.In(datasource.ChinaMarketLocation()).Format(time.DateOnly)).
		Order("code, list_date").Limit(maintenanceMaxUniverseRows + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > maintenanceMaxUniverseRows {
		return nil, errors.New("IPO 上市日期超过补采查询硬上限")
	}
	conflicts := make(map[string]bool)
	for _, row := range rows {
		_, listingErr := time.Parse(time.DateOnly, row.ListDate)
		_, applyErr := time.Parse(time.DateOnly, row.ApplyDate)
		if listingErr != nil || applyErr != nil || row.ApplyDate > row.ListDate {
			conflicts[row.Code] = true
			continue
		}
		if previous := listingDates[row.Code]; previous != "" && previous != row.ListDate {
			conflicts[row.Code] = true
		}
		listingDates[row.Code] = row.ListDate
	}
	for symbol := range conflicts {
		delete(listingDates, symbol)
	}
	return listingDates, nil
}

type dailyBackfillCell struct {
	Symbol          string
	TradeDate       string
	UpdatedAt       time.Time
	Source          string
	BasisUnverified bool
}

func dailyBackfillCellsDB(database *gorm.DB, table any, market string, symbols, dates []string, suspendedOnly bool) (map[string]bool, error) {
	cells := make(map[string]bool)
	if len(symbols) == 0 || len(dates) == 0 {
		return cells, nil
	}
	maxCells := len(symbols) * len(dates)
	query := database.Model(table).Select("symbol", "trade_date").
		Where("market = ? AND symbol IN ? AND trade_date IN ?", market, symbols, dates)
	orderColumns := "symbol, trade_date"
	dailyBars := false
	switch table.(type) {
	case model.DailyBar, *model.DailyBar:
		dailyBars = true
		// A completed candle is still unusable after a failed price-basis check.
		// Read both facts together so plans and write verification agree with readers.
		query = database.Model(table).
			Select("daily_bars.symbol, daily_bars.trade_date, daily_bars.updated_at, daily_bars.source, COALESCE(daily_bar_write_locks.basis_unverified, false) AS basis_unverified").
			Joins("LEFT JOIN daily_bar_write_locks ON daily_bar_write_locks.market = daily_bars.market AND daily_bar_write_locks.symbol = daily_bars.symbol").
			Where("daily_bars.market = ? AND daily_bars.symbol IN ? AND daily_bars.trade_date IN ?", market, symbols, dates)
		orderColumns = "daily_bars.symbol, daily_bars.trade_date"
	}
	if suspendedOnly {
		query = query.Where("suspended = ?", true)
	}
	var rows []dailyBackfillCell
	if err := query.Order(orderColumns).Limit(maxCells + 1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) > maxCells {
		return nil, errors.New("日线覆盖查询超过硬上限")
	}
	for _, row := range rows {
		verified := true
		if dailyBars {
			verified = !row.BasisUnverified && isVerifiedCompletedDailyRow(model.DailyBar{Market: market, TradeDate: row.TradeDate, UpdatedAt: row.UpdatedAt, Source: row.Source})
		}
		// False preserves row presence for contradictory IPO evidence, but is
		// never coverage. Repair plans and hashes use only verified true cells.
		cells[row.Symbol+"@"+row.TradeDate] = verified
	}
	return cells, nil
}

func buildDailyBackfillRecoveryPlanAtDB(database *gorm.DB, request MaintenanceRequest, now time.Time) (*preparedMaintenancePlan, error) {
	market, dates, from, to, err := dailyBackfillOpenDatesAtDB(database, request, now)
	if err != nil {
		return nil, err
	}
	scope, _ := normalizeDailyBackfillScope(request.Scope)
	universe, err := dailyBackfillUniverseDB(database, market, scope)
	if err != nil {
		return nil, err
	}
	if scope == DailyBackfillScopeMarketwide && len(universe) == 0 {
		return nil, errors.New("本地尚无全市场或已跟踪标的，请先执行全市场增量同步建立宇宙；日线预演不会扫描上游")
	}
	listingDates, err := dailyBackfillListingDatesDB(database, now)
	if err != nil {
		return nil, err
	}
	prepared := &preparedMaintenancePlan{dates: dates, missingKeys: make(map[string]struct{})}
	view := MaintenancePlan{Task: MaintenanceSyncBars, Market: market, Scope: scope, UniverseCount: len(universe), From: from, To: to, WindowDays: len(dates), GeneratedAt: now.Format(time.RFC3339)}
	state := maintenanceHashState{Version: "daily-recovery-v3", Task: MaintenanceSyncBars, Market: market, Scope: scope, From: from, To: to, Dates: dates}
	for _, stock := range universe {
		state.Stocks = append(state.Stocks, stock.Symbol)
	}
	// Bound transient memory to 200 * 250 cells per read. Hash per-stock facts
	// rather than retaining up to 7000 * 250 rows in the response or audit log.
	for offset := 0; offset < len(universe); offset += dailyBackfillReadBatchSize {
		batch := universe[offset:min(offset+dailyBackfillReadBatchSize, len(universe))]
		symbols := make([]string, 0, len(batch))
		for _, stock := range batch {
			symbols = append(symbols, stock.Symbol)
		}
		existing, err := dailyBackfillCellsDB(database, &model.DailyBar{}, market, symbols, dates, false)
		if err != nil {
			return nil, err
		}
		suspended, err := dailyBackfillCellsDB(database, &model.StockUniverseDaily{}, market, symbols, dates, true)
		if err != nil {
			return nil, err
		}
		for _, stock := range batch {
			listingDate := listingDates[stock.Symbol]
			for _, date := range dates {
				if _, present := existing[stock.Symbol+"@"+date]; listingDate != "" && date < listingDate && present {
					listingDate = "" // Actual pre-listing bars contradict the IPO record.
				}
			}
			facts := maintenanceHashState{Stocks: []string{stock.Symbol}, LocalState: []string{listingDate}}
			expectedCount, existingCount, suspendedCount := 0, 0, 0
			for _, date := range dates {
				key := stock.Symbol + "@" + date
				if existing[key] {
					facts.Existing = append(facts.Existing, date)
				}
				if suspended[key] {
					facts.Suspended = append(facts.Suspended, date)
				}
				if listingDate != "" && date < listingDate {
					continue
				}
				if suspended[key] {
					suspendedCount++
					continue
				}
				expectedCount++
				if existing[key] {
					existingCount++
				} else {
					facts.Missing = append(facts.Missing, date)
				}
			}
			state.LocalState = append(state.LocalState, hashMaintenanceState(facts))
			if len(facts.Missing) == 0 {
				continue
			}
			if len(prepared.stocks) >= syncMaxStocks {
				view.Capped = true
				continue
			}
			prepared.stocks = append(prepared.stocks, stock)
			for _, date := range facts.Missing {
				prepared.missingKeys[stock.Symbol+"@"+date] = struct{}{}
			}
			view.ExpectedCount += expectedCount
			view.ExistingCount += existingCount
			view.SuspendedCount += suspendedCount
			if len(view.SampleTargets) < 12 {
				view.SampleTargets = append(view.SampleTargets, stock.Symbol)
			}
		}
	}
	view.TargetCount, view.EstimatedRequests, view.MissingCount = len(prepared.stocks), len(prepared.stocks), len(prepared.missingKeys)
	scopeLabel := "已跟踪集合"
	if scope == DailyBackfillScopeMarketwide {
		scopeLabel = "本地已知全市场宇宙 + 已跟踪集合（去重）"
	}
	view.DifferenceSummary = fmt.Sprintf("%s %d 只；窗口 %d 个交易日，本轮 %d 只标的应有 %d 个股票交易日，已核验 %d，缺失/待核验 %d，排除停牌 %d。每轮最多 800 只，完整标的自动跳过；仅可信 IPO 上市日前日期可排除，首次入库日期不作上市依据", scopeLabel, len(universe), len(dates), view.TargetCount, view.ExpectedCount, view.ExistingCount, view.MissingCount, view.SuspendedCount)
	if view.Capped {
		view.DifferenceSummary += "；仍有其他缺失/待核验标的，请本轮结束后重新预演下一轮"
	}
	view.PlanHash = hashMaintenanceState(state)
	prepared.view = view
	return prepared, nil
}

func dailyBackfillFetchLimitAt(dates []string, now time.Time) (int, error) {
	if len(dates) == 0 {
		return 0, errors.New("补采缺少交易日")
	}
	first, err := time.ParseInLocation(time.DateOnly, dates[0], datasource.ChinaMarketLocation())
	if err != nil {
		return 0, err
	}
	// A natural-day upper bound also reaches old explicit windows when the
	// newer calendar is incomplete. Never underfetch the adjustment reference.
	now = now.In(datasource.ChinaMarketLocation())
	minimum := dailyBackfillMaxOpenDays
	if now.Hour()*60+now.Minute() < sessionCloseMin+5 {
		minimum++ // An unfinished current candle may consume one source row.
	}
	limit := max(minimum, int(now.Sub(first).Hours()/24)+2)
	if limit > 1000 {
		return 0, errors.New("日线回看超过 1000 根请求硬上限，请重新选择范围")
	}
	return limit, nil
}

// Count only dates actually present after the transaction. A source may return
// duplicates or trigger a rebase that saves fewer dates than its response size.
func dailyBackfillPersistedCoverageDB(database *gorm.DB, stock model.Stock, missingDates []string) (saved, remaining int, err error) {
	cells, err := dailyBackfillCellsDB(database, &model.DailyBar{}, stock.Market, []string{stock.Symbol}, missingDates, false)
	if err != nil {
		return 0, len(missingDates), err
	}
	for _, verified := range cells {
		if verified {
			saved++
		}
	}
	return saved, len(missingDates) - saved, nil
}

func dailyBackfillExpectedCompletedDateDB(database *gorm.DB, now time.Time) (string, error) {
	reader := &dataHealthReader{db: database, now: now}
	expected := reader.prevOpenTradeDate(dailyBackfillCompletedCutoff(now).AddDate(0, 0, 1).Format(time.DateOnly))
	if reader.err != nil {
		return "", reader.err
	}
	if expected == "" {
		return "", errors.New("交易日历不完整，无法确认应有收盘日；已保存的历史不代表同步完整，请先回填日历")
	}
	return expected, nil
}

func dailyBackfillCompletedBars(bars []datasource.Bar, market string, now time.Time) []datasource.Bar {
	if market != "cn" {
		return bars
	}
	cutoff := dailyBackfillCompletedCutoff(now).Format(time.DateOnly)
	completed := make([]datasource.Bar, 0, len(bars))
	for _, bar := range bars {
		if bar.TradeDate != "" && bar.TradeDate <= cutoff {
			completed = append(completed, bar)
		}
	}
	return completed
}

func dailyBackfillVerifyCompletedDB(database *gorm.DB, stock model.Stock, expectedDate string, fresh bool) error {
	if expectedDate == "" {
		return errors.New("应有收盘日期未知，不能确认同步成功")
	}
	var suspendedCount int64
	if err := database.Model(&model.StockUniverseDaily{}).
		Where("market = ? AND symbol = ? AND trade_date = ? AND suspended = ?", stock.Market, stock.Symbol, expectedDate, true).
		Count(&suspendedCount).Error; err != nil {
		return err
	}
	if suspendedCount > 0 {
		return nil
	}
	_, remaining, err := dailyBackfillPersistedCoverageDB(database, stock, []string{expectedDate})
	if err != nil {
		return err
	}
	if remaining > 0 || !fresh {
		return fmt.Errorf("来源未提供应有收盘日 %s 的完整日线且无已确认停牌；有效历史已保留，最新日仍待补", expectedDate)
	}
	return nil
}
