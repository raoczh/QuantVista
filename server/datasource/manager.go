package datasource

import (
	"context"
	"errors"

	"quantvista/common"
)

// Manager 按优先级编排多个 Adapter：主源失败时回退到下一个，
// 上层只依赖内部标准结构，单源挂掉可整体切换（见 docs/DATA_SOURCES.md）。
type Manager struct {
	adapters []Adapter // 按优先级排列，[0] 为主源
}

// DefaultManager 默认编排：东财为主（数据最全），腾讯次之（稳定），新浪兜底（含日线/指数/榜单）。
func DefaultManager() *Manager {
	return &Manager{
		adapters: []Adapter{
			NewEastMoneyAdapter(),
			NewTencentAdapter(),
			NewSinaAdapter(),
		},
	}
}

// GetQuote 依次尝试各源，返回首个成功结果（含实际命中的 Source）。
// 单个源失败只记 DEBUG（有备源兜底不必刷屏）；全部失败才记 WARN。
func (m *Manager) GetQuote(ctx context.Context, market, symbol string) (*Quote, error) {
	var lastErr error
	for _, a := range m.adapters {
		q, err := a.GetQuote(ctx, market, symbol)
		if err == nil {
			return q, nil
		}
		// 代码非法/不支持该市场无需换源重试。
		if errors.Is(err, ErrSymbolInvalid) {
			return nil, err
		}
		if !errors.Is(err, ErrNotSupported) {
			common.SysDebug("数据源 %s 取行情失败 symbol=%s: %v", a.Name(), symbol, err)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	common.SysWarn("所有数据源取行情失败 symbol=%s: %v", symbol, lastErr)
	return nil, lastErr
}

// GetDailyBars 依次尝试支持日线的源（新浪返回 ErrNotSupported 时回退东财）。
func (m *Manager) GetDailyBars(ctx context.Context, market, symbol string, limit int) ([]Bar, error) {
	var lastErr error
	for _, a := range m.adapters {
		bars, err := a.GetDailyBars(ctx, market, symbol, limit)
		if err == nil {
			return bars, nil
		}
		if errors.Is(err, ErrSymbolInvalid) {
			return nil, err
		}
		if !errors.Is(err, ErrNotSupported) {
			common.SysDebug("数据源 %s 取日线失败 symbol=%s: %v", a.Name(), symbol, err)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	common.SysWarn("所有数据源取日线失败 symbol=%s: %v", symbol, lastErr)
	return nil, lastErr
}

// GetIndices 路由到实现 IndexProvider 的源（新浪批量优先）。
func (m *Manager) GetIndices(ctx context.Context, market string) ([]Index, error) {
	var lastErr error = ErrNotSupported
	for _, a := range m.adapters {
		p, ok := a.(IndexProvider)
		if !ok {
			continue
		}
		r, err := p.GetIndices(ctx, market)
		if err == nil {
			return r, nil
		}
		common.SysDebug("数据源 %s 取指数失败: %v", a.Name(), err)
		lastErr = err
	}
	return nil, lastErr
}

// GetStockRanking 路由到实现 RankingProvider 的源（新浪 Market_Center）。
func (m *Manager) GetStockRanking(ctx context.Context, market, sort string, limit int) ([]StockRank, error) {
	var lastErr error = ErrNotSupported
	for _, a := range m.adapters {
		p, ok := a.(RankingProvider)
		if !ok {
			continue
		}
		r, err := p.GetStockRanking(ctx, market, sort, limit)
		if err == nil {
			return r, nil
		}
		common.SysDebug("数据源 %s 取榜单失败 sort=%s: %v", a.Name(), sort, err)
		lastErr = err
	}
	return nil, lastErr
}

// GetSectorRanking 路由到实现 SectorProvider 的源（东财 clist，best-effort）。
func (m *Manager) GetSectorRanking(ctx context.Context, market string, limit int) ([]SectorRank, error) {
	var lastErr error = ErrNotSupported
	for _, a := range m.adapters {
		p, ok := a.(SectorProvider)
		if !ok {
			continue
		}
		r, err := p.GetSectorRanking(ctx, market, limit)
		if err == nil {
			return r, nil
		}
		common.SysDebug("数据源 %s 取板块榜失败: %v", a.Name(), err)
		lastErr = err
	}
	return nil, lastErr
}
