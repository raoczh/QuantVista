package service

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// daily_bars 保留期清理。此前没有任何按日期的删除路径：每日 clist 增量约 5500 行、
// 每年约 135 万行，只增不减；只有除权重锚会整股删插重建。本文件封住这个增长。
//
// 为什么按市场分批删、而不是一条 DELETE 了事：
//   - 一条 `DELETE WHERE trade_date < ?` 首轮要删几十万行，InnoDB 会把这些行全锁住并
//     写进一个巨大的 undo/binlog 事务；期间盘后同步与因子任务撞上同一批页面就会等锁。
//     分批（每批 barRetentionBatchRows）让每个事务都短，锁随批次释放。
//   - 带上 market 才能命中 idx_bar_market_date (market, trade_date)。裸 trade_date 条件
//     没有可用索引前缀，会退化成全表扫。
const (
	// barRetentionBatchRows 单批删除行数。5000 行让单个事务足够短，同时批次数不至于太多。
	barRetentionBatchRows = 5000
	// barRetentionMaxBatches 单轮批次上限，兼作失控保护。5000 × 2000 = 1000 万行，
	// 远超首轮预期删除量（约 60 万行）；到顶仍有剩余则留给下一轮，不无限占用连接。
	barRetentionMaxBatches = 2000
)

// CleanupDailyBarsBefore 删除 trade_date 早于 cutoff 的日线，返回删除行数。
// cutoff 为空时用 model.DailyBarRetentionCutoff()。幂等：重复执行只删剩余超期行。
func CleanupDailyBarsBefore(cutoff string) (int64, error) {
	if common.DB == nil {
		return 0, errors.New("数据库尚未初始化")
	}
	if cutoff == "" {
		cutoff = model.DailyBarRetentionCutoff()
	}
	// 防呆：cutoff 必须是合法日期。空串或格式错会让条件退化成删掉几乎所有行。
	if _, err := time.Parse("2006-01-02", cutoff); err != nil {
		return 0, fmt.Errorf("保留下限日期非法 %q: %w", cutoff, err)
	}

	markets, err := dailyBarMarkets()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, market := range markets {
		deleted, err := cleanupDailyBarsForMarket(market, cutoff)
		total += deleted
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// dailyBarMarkets 表内实际存在的市场列表。取自数据而非硬编码 "cn"：将来接入其他市场
// 后清理会自动覆盖，不会漏掉一个只增不减的市场。
func dailyBarMarkets() ([]string, error) {
	var markets []string
	if err := common.DB.Model(&model.DailyBar{}).
		Distinct().Pluck("market", &markets).Error; err != nil {
		return nil, err
	}
	return markets, nil
}

// cleanupDailyBarsForMarket 分批删除单个市场的超期行。
//
// 先取 id 再按主键删，而不是 `DELETE ... LIMIT`：后者只有 MySQL 支持，SQLite（需编译期
// 开启 UPDATE_DELETE_LIMIT）与 Postgres 都不认，而本项目三种方言都要跑（common/database.go）。
// 多一次往返换来的是三端行为一致，且按主键删除走聚簇索引最快。
func cleanupDailyBarsForMarket(market, cutoff string) (int64, error) {
	var deleted int64
	for i := 0; i < barRetentionMaxBatches; i++ {
		var ids []int64
		if err := common.DB.Model(&model.DailyBar{}).
			Where("market = ? AND trade_date < ?", market, cutoff).
			Limit(barRetentionBatchRows).
			Pluck("id", &ids).Error; err != nil {
			return deleted, err
		}
		if len(ids) == 0 {
			return deleted, nil // 本市场已删净
		}
		res := common.DB.Where("id IN ?", ids).Delete(&model.DailyBar{})
		if res.Error != nil {
			return deleted, res.Error
		}
		deleted += res.RowsAffected
		if len(ids) < barRetentionBatchRows {
			return deleted, nil
		}
	}
	common.SysWarn("daily_bars 保留期清理达单轮批次上限（market=%s 已删 %d 行），剩余留给下一轮", market, deleted)
	return deleted, nil
}

var startBarRetentionOnce sync.Once

// StartDailyBarRetentionJob 每日 03:50 清理超期日线。
//
// 时点选择：错在 03:35 的作业事件清理之后、盘前任何同步之前。绝不能和盘后链路
// （16:xx 快照/峰值、19:xx 财报与公司行动、19:35 守护轮）重叠——那些任务正在读写
// 同一张表。凌晨也避开了除权重锚（随盘后同步触发）整股删插的窗口。
func StartDailyBarRetentionJob() {
	startBarRetentionOnce.Do(func() {
		go func() {
			for {
				now := time.Now()
				next := time.Date(now.Year(), now.Month(), now.Day(), 3, 50, 0, 0, now.Location())
				if !next.After(now) {
					next = next.AddDate(0, 0, 1)
				}
				time.Sleep(next.Sub(now))
				cutoff := model.DailyBarRetentionCutoff()
				if deleted, err := CleanupDailyBarsBefore(cutoff); err != nil {
					common.SysWarn("daily_bars 保留期清理失败（下限 %s）: %v", cutoff, err)
				} else if deleted > 0 {
					common.SysLog("daily_bars 保留期清理完成：删除 %d 行（保留 %d 天，下限 %s）",
						deleted, model.DailyBarRetentionDays, cutoff)
				}
			}
		}()
	})
}

// DailyBarRetentionStat 保留期现状，供管理端只读查看。
type DailyBarRetentionStat struct {
	RetentionDays int    `json:"retention_days"`
	Cutoff        string `json:"cutoff"`
	TotalRows     int64  `json:"total_rows"`
	StaleRows     int64  `json:"stale_rows"` // 早于 cutoff、待清理的行数
	MinTradeDate  string `json:"min_trade_date"`
	MaxTradeDate  string `json:"max_trade_date"`
}

// GetDailyBarRetentionStat 只读统计。COUNT(*) 在 InnoDB 上是全索引扫描，行数百万级时
// 耗时可观，故仅供管理端按需调用，不进任何高频路径。
func GetDailyBarRetentionStat() (*DailyBarRetentionStat, error) {
	if common.DB == nil {
		return nil, errors.New("数据库尚未初始化")
	}
	cutoff := model.DailyBarRetentionCutoff()
	out := &DailyBarRetentionStat{RetentionDays: model.DailyBarRetentionDays, Cutoff: cutoff}
	var agg struct {
		Total   int64
		MinDate string
		MaxDate string
	}
	if err := common.DB.Model(&model.DailyBar{}).
		Select("COUNT(*) AS total, MIN(trade_date) AS min_date, MAX(trade_date) AS max_date").
		Scan(&agg).Error; err != nil {
		return nil, err
	}
	out.TotalRows, out.MinTradeDate, out.MaxTradeDate = agg.Total, agg.MinDate, agg.MaxDate
	if err := common.DB.Model(&model.DailyBar{}).
		Where("trade_date < ?", cutoff).Count(&out.StaleRows).Error; err != nil {
		return nil, err
	}
	return out, nil
}
