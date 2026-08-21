package model

import "time"

// Stock 股票基础信息（多市场）。
type Stock struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Symbol    string    `gorm:"size:16;index:idx_symbol_market,unique" json:"symbol"` // 如 600000
	Market    string    `gorm:"size:8;index:idx_symbol_market,unique" json:"market"`  // cn/us/hk
	Name      string    `gorm:"size:64" json:"name"`
	Industry  string    `gorm:"size:64" json:"industry"`
	Currency  string    `gorm:"size:8;default:CNY" json:"currency"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StockQuote 最新行情快照（每 symbol+market 一条，覆盖更新）。
// 价格用 decimal 列类型，避免浮点累计误差（骨架以 float64 承载，后续可换 decimal 库）。
type StockQuote struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Symbol    string    `gorm:"size:16;index:idx_q_symbol_market,unique" json:"symbol"`
	Market    string    `gorm:"size:8;index:idx_q_symbol_market,unique" json:"market"`
	Price     float64   `gorm:"type:decimal(20,4)" json:"price"`
	ChangePct float64   `gorm:"type:decimal(10,4)" json:"change_pct"`
	Open      float64   `gorm:"type:decimal(20,4)" json:"open"`
	High      float64   `gorm:"type:decimal(20,4)" json:"high"`
	Low       float64   `gorm:"type:decimal(20,4)" json:"low"`
	PrevClose float64   `gorm:"type:decimal(20,4)" json:"prev_close"`
	Volume    int64     `json:"volume"`
	Amount    float64   `gorm:"type:decimal(24,4)" json:"amount"`
	Source    string    `gorm:"size:16" json:"source"` // 数据来源：eastmoney/sina
	DataTime  time.Time `json:"data_time"`             // 数据时间，AI 分析需明确告知
	UpdatedAt time.Time `json:"updated_at"`
}

// DailyBar 日线 OHLC，供追踪/回撤/复权计算复用。
//
// 三个索引各有不可替代的职责，**都不能删**：
//   - idx_bar_symbol_date (symbol, market, trade_date) UNIQUE：既是单股取序列的主索引，
//     也是 market.go / marketwide.go 两处 upsert 的 ON CONFLICT 冲突目标。缺失或丢掉
//     UNIQUE 时每日同步不报错、而是静默插入重复行，属于正确性依赖（cmd/barscheck 可核对）。
//   - idx_bar_market_date (market, trade_date)：服务只按市场+日期、**不带 symbol** 的查询
//     ——数据健康缺口检查（datahealth.go）、宇宙 join、候选审计、以及保留期清理的
//     WHERE market=? AND trade_date < ?。这些用不上下面那个新索引（symbol 挡在中间）。
//   - idx_market_symbol_date (market, symbol, trade_date)：给四处全市场流式读
//     （buildFactorTable / RunFactorIC / streamCNDailyBars / M2 回测）消除 filesort。
//     线上 EXPLAIN 实测：WHERE market='cn' ORDER BY symbol, trade_date 走
//     idx_bar_market_date 过滤后仍要 Using filesort 排 27 万+ 行，单次 4~9 秒；本索引
//     的列序恰好等于「过滤列 + 排序列」，B+Tree 天然有序，排序整段消掉。仍需回表取
//     OHLC 各列，所以不会到毫秒级，但流式读不必再等全量排序完才拿到第一行。
type DailyBar struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	Symbol string `gorm:"size:16;index:idx_bar_symbol_date,unique;index:idx_market_symbol_date,priority:2" json:"symbol"`
	Market string `gorm:"size:8;index:idx_bar_symbol_date,unique;index:idx_bar_market_date,priority:1;index:idx_market_symbol_date,priority:1" json:"market"`
	// TradeDate YYYY-MM-DD
	TradeDate string  `gorm:"size:10;index:idx_bar_symbol_date,unique;index:idx_bar_market_date,priority:2;index:idx_market_symbol_date,priority:3" json:"trade_date"`
	Open      float64 `gorm:"type:decimal(20,4)" json:"open"`
	High      float64 `gorm:"type:decimal(20,4)" json:"high"`
	Low       float64 `gorm:"type:decimal(20,4)" json:"low"`
	Close     float64 `gorm:"type:decimal(20,4)" json:"close"`
	Volume    int64   `json:"volume"`
	Amount    float64 `gorm:"type:decimal(24,4)" json:"amount"`
	// TurnoverRate 当日换手率 %（东财日线 f61 自带；新浪兜底无此字段，0=缺失）。
	// 供筹码分布离线复算与 M1 因子宽表使用。
	TurnoverRate float64   `gorm:"type:decimal(10,4)" json:"turnover_rate"`
	Source       string    `gorm:"size:16" json:"source"`
	CreatedAt    time.Time `json:"created_at"`
}

// DailyBarRetentionDays daily_bars 保留天数（自然日）。
//
// 400 天 ≈ 270 个交易日，是「覆盖全部既有计算窗口」推出来的下限，不是随手取的整数：
// 系统多处硬依赖 250 个交易日的历史——ma250 / pos_250 / 年内新高（factortable.go）、
// 板块估值时序分位（boardValHistWindow）、技术指标与资金流窗口（indicatorMaxLimit /
// fflowBarLimit）、as-of 快照（asOfBarLimit）、以及除权重锚重建的 wideBarLimit=250。
// 250 个交易日按 A 股每年约 243 个交易日折算约需 375 自然日，400 天留出约 25 天余量
// 兜住长假与停牌。**下调此值会静默改变上述因子的口径**（窗口不足时按现有样本算，
// 不会报错），调整前必须同步评估那些窗口。
const DailyBarRetentionDays = 400

// DailyBarRetentionCutoff 保留下限日期（YYYY-MM-DD）：早于此日期的日线可被清理。
// 与清理任务、体检工具（cmd/barscheck）共用同一口径，避免两处各算一次而漂移。
func DailyBarRetentionCutoff() string {
	return DailyBarRetentionCutoffAt(time.Now())
}

// DailyBarRetentionCutoffAt 按给定基准时刻算保留下限，便于测试注入固定时间。
func DailyBarRetentionCutoffAt(now time.Time) string {
	return now.AddDate(0, 0, -DailyBarRetentionDays).Format("2006-01-02")
}

// TradingCalendar 交易日历，用于按交易日计算有效期、持有周期和数据新鲜度。
type TradingCalendar struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	Market    string `gorm:"size:8;index:idx_calendar_market_date,unique" json:"market"`
	TradeDate string `gorm:"size:10;index:idx_calendar_market_date,unique" json:"trade_date"` // YYYY-MM-DD
	// 不设 gorm default:true——GORM 会把零值(false)当作"用默认值"而从 INSERT 中省略，
	// 导致回填的休市日被 DB 默认值写成 true。去掉 default，false 才会被真实写入。
	IsOpen    bool      `json:"is_open"`
	CreatedAt time.Time `json:"created_at"`
}

// MarketSnapshot 市场情绪快照：涨跌家数、涨跌停。
// 定时落库形成历史序列，供首页情绪卡片与后续 AI 市场分析复用（明确带来源与数据时间）。
type MarketSnapshot struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	Market    string    `gorm:"size:8;index:idx_snap_market_time" json:"market"`
	TradeDate string    `gorm:"size:10;index:idx_snap_market_date" json:"trade_date"` // YYYY-MM-DD，快照对应的交易日
	Advances  int       `json:"advances"`                                             // 上涨家数
	Declines  int       `json:"declines"`                                             // 下跌家数
	Unchanged int       `json:"unchanged"`                                            // 平盘家数
	LimitUp   int       `json:"limit_up"`                                             // 涨停家数
	LimitDown int       `json:"limit_down"`                                           // 跌停家数
	Source    string    `gorm:"size:16" json:"source"`
	DataTime  time.Time `gorm:"index:idx_snap_market_time" json:"data_time"`
	CreatedAt time.Time `json:"created_at"`
}

// DataSyncLog 数据同步任务审计：记录批量日线同步/日历回填等后台任务的执行结果。
// 便于排查数据缺口与数据源限流（对应 phase0 review P1#3 的 data_sync_logs 缺口）。
type DataSyncLog struct {
	ID         int64  `gorm:"primaryKey" json:"id"`
	JobRunID   *int64 `gorm:"uniqueIndex" json:"job_run_id,omitempty"`
	Task       string `gorm:"size:32;index;index:idx_sync_task_created,priority:1" json:"task"` // sync_daily_bars / backfill_calendar / snapshot_market
	Market     string `gorm:"size:8" json:"market"`
	Status     string `gorm:"size:16" json:"status"` // processing / success / partial / failed / canceled
	Total      int    `json:"total"`                 // 计划处理条目数
	Succeeded  int    `json:"succeeded"`             // 成功条目数
	Failed     int    `json:"failed"`                // 失败条目数
	DurationMs int64  `json:"duration_ms"`           // 耗时（毫秒）
	Message    string `gorm:"size:512" json:"message"`

	// 运维审计只存白名单摘要，不存请求正文、token、cookie 或上游响应。
	TriggerSource    string    `gorm:"size:24;index;default:scheduler" json:"trigger_source"` // scheduler / startup / admin / admin_legacy
	UserID           int64     `gorm:"index" json:"user_id"`                                  // 管理员手动触发者；系统任务为 0
	ParameterSummary string    `gorm:"size:1024" json:"parameter_summary"`
	RangeSummary     string    `gorm:"size:128" json:"range_summary"`
	PlanHash         string    `gorm:"size:64" json:"plan_hash"`
	CreatedAt        time.Time `gorm:"index;index:idx_sync_task_created,priority:2" json:"created_at"`
}
