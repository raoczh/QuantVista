package datasource

import (
	"context"
	"strings"
)

func boardHeatCapability(kind string) (string, bool) {
	switch kind {
	case "industry":
		return "board_industry", true
	case "concept":
		return "board_concept", true
	default:
		return "", false
	}
}

// BoardCodeSource validates and identifies a classification without guessing
// relationships between suppliers. Existing BK deep links keep their meaning.
func BoardCodeSource(code string) (string, bool) {
	if bkCodeRe.MatchString(strings.ToUpper(strings.TrimSpace(code))) {
		return "eastmoney", true
	}
	if _, valid := sinaBoardNode(code); valid {
		return "sina", true
	}
	return "", false
}

func (manager *Manager) GetBoardHeat(ctx context.Context, kind string) ([]BoardHeat, error) {
	capability, valid := boardHeatCapability(kind)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	return routeCap(manager, ctx, "cn", capability, func(sourceContext context.Context, adapter Adapter) ([]BoardHeat, error) {
		provider, supported := adapter.(BoardHeatProvider)
		if !supported {
			return nil, ErrNotSupported
		}
		rows, err := provider.GetBoardHeatLimited(sourceContext, kind, boardHeatPageSize)
		if err == nil && len(rows) == 0 {
			err = ErrNoData
		}
		return rows, err
	})
}

func (manager *Manager) GetBoardConstituents(ctx context.Context, code string, limit int) ([]BoardStock, error) {
	source, valid := BoardCodeSource(code)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	return routeCap(manager, ctx, "cn", "board_stocks", func(sourceContext context.Context, adapter Adapter) ([]BoardStock, error) {
		provider, supported := adapter.(BoardConstituentsProvider)
		if !supported || adapter.Name() != source {
			return nil, ErrNotSupported
		}
		rows, err := provider.GetBoardConstituents(sourceContext, code, limit)
		if err == nil && len(rows) == 0 {
			err = ErrNoData
		}
		return rows, err
	})
}

func (manager *Manager) GetBoardKline(ctx context.Context, code string, limit int) ([]Bar, error) {
	source, valid := BoardCodeSource(code)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	if source != "eastmoney" {
		return nil, ErrNotSupported
	}
	return routeCap(manager, ctx, "cn", "board_bars", func(sourceContext context.Context, adapter Adapter) ([]Bar, error) {
		provider, supported := adapter.(BoardHistoryProvider)
		if !supported || adapter.Name() != source {
			return nil, ErrNotSupported
		}
		rows, err := provider.GetBoardKline(sourceContext, code, limit)
		if err == nil && len(rows) == 0 {
			err = ErrNoData
		}
		return rows, err
	})
}

func (manager *Manager) GetBoardFundFlow(ctx context.Context, code string, limit int) ([]StockFundFlowBar, error) {
	source, valid := BoardCodeSource(code)
	if !valid {
		return nil, ErrSymbolInvalid
	}
	if source != "eastmoney" {
		return nil, ErrNotSupported
	}
	return routeCap(manager, ctx, "cn", "board_fundflow", func(sourceContext context.Context, adapter Adapter) ([]StockFundFlowBar, error) {
		provider, supported := adapter.(BoardHistoryProvider)
		if !supported || adapter.Name() != source {
			return nil, ErrNotSupported
		}
		rows, err := provider.GetBoardFundFlow(sourceContext, code, limit)
		if err == nil && len(rows) == 0 {
			err = ErrNoData
		}
		return rows, err
	})
}
