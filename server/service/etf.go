package service

import (
	"context"
	"strings"
)

// isCNFund 按 A 股基金代码前缀判定是否为场内基金（ETF/LOF/封闭基金/REITs）。
// 前缀口径：沪市 50x（LOF 501/502、老封基 500、REITs 508）/51x/56x/58x ETF、
// 深市 ETF 15x、LOF 16x、封闭基金 18x。沪市 50 开头无个股/可转债，放入安全；
// 与 datasource.cnSecid/sinaCNSymbol 放行的深市基金前缀（15/16/18）保持一致，
// 收敛到具体两位前缀是为避免误判可转债（10x/11x）。
func isCNFund(symbol string) bool {
	s := strings.TrimSpace(symbol)
	if len(s) != 6 {
		return false
	}
	switch s[:2] {
	case "50", "51", "56", "58", "15", "16", "18":
		return true
	default:
		return false
	}
}

// EtfItem 指数 ETF 清单条目（清单为硬编码精选，行情按需实时富化）。
type EtfItem struct {
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name"`
	Index     string  `json:"index"`    // 跟踪指数
	Category  string  `json:"category"` // 宽基 / 行业主题 / 商品跨境
	Price     float64 `json:"price"`
	ChangePct float64 `json:"change_pct"`
	QuoteOK   bool    `json:"quote_ok"`
}

// etfCatalog 精选主流指数 ETF（全部沪深市场）。顺序即前端展示顺序：宽基→行业主题→商品跨境。
var etfCatalog = []EtfItem{
	// 宽基
	{Symbol: "510050", Name: "上证50ETF", Index: "上证50", Category: "宽基"},
	{Symbol: "510300", Name: "沪深300ETF", Index: "沪深300", Category: "宽基"},
	{Symbol: "510500", Name: "中证500ETF", Index: "中证500", Category: "宽基"},
	{Symbol: "512100", Name: "中证1000ETF", Index: "中证1000", Category: "宽基"},
	{Symbol: "588000", Name: "科创50ETF", Index: "科创50", Category: "宽基"},
	{Symbol: "159915", Name: "创业板ETF", Index: "创业板指", Category: "宽基"},
	{Symbol: "159949", Name: "创业板50ETF", Index: "创业板50", Category: "宽基"},
	// 行业主题
	{Symbol: "512880", Name: "证券ETF", Index: "中证全指证券公司", Category: "行业主题"},
	{Symbol: "512010", Name: "医药ETF", Index: "沪深300医药卫生", Category: "行业主题"},
	{Symbol: "512690", Name: "酒ETF", Index: "中证酒", Category: "行业主题"},
	{Symbol: "512480", Name: "半导体ETF", Index: "中证全指半导体", Category: "行业主题"},
	{Symbol: "515000", Name: "科技ETF", Index: "中证科技龙头", Category: "行业主题"},
	{Symbol: "510880", Name: "红利ETF", Index: "上证红利", Category: "行业主题"},
	// 商品跨境
	{Symbol: "518880", Name: "黄金ETF", Index: "上海金", Category: "商品跨境"},
	{Symbol: "513100", Name: "纳指ETF", Index: "纳斯达克100", Category: "商品跨境"},
	{Symbol: "513050", Name: "中概互联ETF", Index: "中国互联网50", Category: "商品跨境"},
}

// EtfService 指数 ETF 清单 + 实时行情富化。市场统一为 A 股（cn）。
type EtfService struct {
	market *MarketService
}

func NewEtfService(market *MarketService) *EtfService {
	return &EtfService{market: market}
}

// List 返回精选 ETF 清单并附实时行情：批量取现价/涨跌幅，单只失败降级为缺席（quote_ok=false）。
func (s *EtfService) List(ctx context.Context) []EtfItem {
	items := make([]EtfItem, len(etfCatalog))
	copy(items, etfCatalog)

	refs := make([]QuoteRef, 0, len(items))
	for _, it := range items {
		refs = append(refs, QuoteRef{Market: "cn", Symbol: it.Symbol})
	}
	quotes := s.market.QuotesFor(ctx, refs)
	for i := range items {
		if q := quotes[QuoteKey("cn", items[i].Symbol)]; q != nil {
			items[i].Price = round4(q.Price) // ETF 最小变动价位 0.001，round2 会抹掉第三位小数
			items[i].ChangePct = round2(q.ChangePct)
			items[i].QuoteOK = true
		}
	}
	return items
}
