package datasource

import "errors"

// 适配层通用错误。
var (
	// ErrNotSupported 该数据源不支持此能力，上层据此决定降级/切换。
	ErrNotSupported = errors.New("数据源不支持该能力")
	// ErrSymbolInvalid 股票代码非法或无法识别市场。
	ErrSymbolInvalid = errors.New("非法的股票代码")
	// ErrUpstream 上游数据源返回异常（限流、字段缺失、网络等）。
	ErrUpstream = errors.New("上游数据源异常")
	// ErrNoData 上游正常但无对应数据。
	ErrNoData = errors.New("无数据")
)
