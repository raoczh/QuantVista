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
	// Probe errors are deliberately coarse.  They are safe to expose to an
	// administrator and must never contain an upstream response body.
	ErrProbeNotAllowed  = errors.New("探测能力未注册")
	ErrProbeRateLimited = errors.New("探测请求过于频繁")
	ErrProbeBusy        = errors.New("探测并发已满")
	ErrProbeUnavailable = errors.New("探测数据源不可用")
)
