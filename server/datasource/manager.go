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

// DefaultManager 默认编排：东财为主，新浪为辅。
func DefaultManager() *Manager {
	return &Manager{
		adapters: []Adapter{
			NewEastMoneyAdapter(),
			NewSinaAdapter(),
		},
	}
}

// GetQuote 依次尝试各源，返回首个成功结果（含实际命中的 Source）。
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
			common.SysWarn("数据源 %s 取行情失败 symbol=%s: %v", a.Name(), symbol, err)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
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
			common.SysWarn("数据源 %s 取日线失败 symbol=%s: %v", a.Name(), symbol, err)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ErrNoData
	}
	return nil, lastErr
}
