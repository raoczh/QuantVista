package model

import "time"

// FactorSnapshotDaily 每日因子宽表快照（S3-1 point-in-time 不可变快照）。
//
// 目的：daily_bars 是前复权序列，除权重锚（rebaseStock）会整股删插重写历史——
// 事后从 daily_bars 重算的历史因子值 ≠ 当时实际可见的值。本表在每日 16:10 增量
// 成功、宽表重建完成后把「当时算出的因子值」固化落库，为 S3~S5 的历史回放/
// walk-forward 提供严格 PIT 数据。禁止历史回测临时调「当前最新接口」填旧日期。
//
// 与既有数据的关系：
//   - 因子宽表内存缓存（factortable.go）：本表是它的逐日持久化影子，消费端后置；
//   - 宇宙快照 stock_universe_dailies（S0-3）：存当日宇宙状态与估值（clist 口径），
//     本表存 52 项技术/量价因子（daily_bars 重算口径），两表按 (trade_date, symbol) 天然可 join；
//   - A 类因子理论上可由日线重建（S3-4 IC 即如此），本表的增量价值是「免疫复权重写
//     的不可变性」与未来 C 类因子并入时的统一落点。
//
// 存储设计：一股一日一行，因子按 JSON 存（NaN 缺失项不落键，round2/3 已裁精度）。
// 实测约 0.8KB/行 × 5150 行/日 ≈ 4MB/日、约 1GB/年，个人库可承受；
// 列式建 52 物理列会锁死因子演进（加因子要 DDL），故取行 JSON。
//
// 不可变纪律：同一 trade_date 只写一次（首写胜），重建/重跑不覆盖——
// 覆盖会把「除权重写后」的值伪装成当时快照，正是本表要防的泄漏。
type FactorSnapshotDaily struct {
	ID        int64  `gorm:"primaryKey" json:"id"`
	TradeDate string `gorm:"size:10;uniqueIndex:idx_fsd_key" json:"trade_date"`
	Symbol    string `gorm:"size:16;uniqueIndex:idx_fsd_key" json:"symbol"`
	Market    string `gorm:"size:8" json:"market"`
	Name      string `gorm:"size:64" json:"name"`

	// LastBarDate 该股末根日线日期；< TradeDate 即当日停牌/数据滞后（stale），
	// 消费端按需排除，落库不排除（停牌本身也是当日事实）。
	LastBarDate string `gorm:"size:10" json:"last_bar_date"`

	// FactorsJSON {"close":10.5,"rsi_14":56.2,...}——键为 factorDefs 的 key，
	// NaN（样本不足/筹码拒算）不落键；布尔因子 1/0。
	FactorsJSON string `gorm:"type:text" json:"factors_json"`

	FactorVersion string    `gorm:"size:8" json:"factor_version"`
	CreatedAt     time.Time `json:"created_at"`
}
