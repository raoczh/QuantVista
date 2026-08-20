package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"

	"gorm.io/gorm/clause"
)

// N2 新闻→AI 信号：LLM 分级情绪增强 + 个股当日聚合情绪分。
//
// 成本控制是设计核心（验收：LLM 增强调用数/新闻数 < 50%）：
//   - P1/P2 源（财联社电报/东财快讯）全量走 LLM 提取 {sentiment, sentiment_score,
//     impact_scope, related_sectors, policy_level}，批量 8 条/次请求摊薄开销；
//   - P3 源（东财个股新闻）先走关键词规则表，仅规则给不出板块时才调简化版 LLM，
//     且每轮上限 newsAISimplifiedCap 条；
//   - P4/P5（未来低优源）纯关键词规则，不碰 LLM；
//   - LLM 不可用（无管理员配置/调用失败）整体降级为规则表，行照样标记已增强，
//     不留「永远重试烧 token」的尾巴。
//
// 两个幂等键分清：逐条新闻增强按 news.id（Sentiment 非空 = 已增强，与 symbol 无关，
// 市场/政策类新闻常无 symbol）；个股当日聚合情绪分按 (symbol, date) 唯一，当日按新闻
// 采集间隔刷新、历史日冻结，并发合并用包级 mutex+map（项目无 x/sync 依赖）。
//
// LLM 挂在首个管理员的默认配置上（新闻是全局共享数据，非用户动作）：
// token 记入该管理员审计、不扣次数配额。

const (
	newsAIBatchSize     = 8  // 一次 LLM 请求携带的新闻条数
	newsAIRoundCap      = 40 // 每轮最多处理的待增强新闻数
	newsAISimplifiedCap = 10 // 每轮 P3 简化版 LLM 调用的新闻条数上限
	newsAIWindow        = 48 * time.Hour
	sentiAggMaxNews     = 12   // 个股当日聚合最多采样的新闻条数
	sentiPromptVersion  = "n3" // n3: 移除紧凑输出措辞；n2: 每条输入恰好一项、禁额外字段与解释的输出瘦身；n1: 初版
)

// newsAILLMCalls / newsAIEnhanced LLM 调用比例监控（验收指标 <50%）。
var (
	newsAILLMCalls int64 // 经 LLM 增强的新闻条数
	newsAIEnhanced int64 // 增强总条数（含规则路径）
)

// newsSectorWhitelist 本地板块白名单：LLM 输出的 related_sectors 逐一对照校验，
// 不在名单内的直接丢弃（防幻觉板块）。口径 = 申万一级行业 + 常见热门概念。
var newsSectorWhitelist = map[string]bool{}

var newsSectorList = []string{
	// 行业（申万一级口径）
	"农林牧渔", "采掘", "煤炭", "石油石化", "化工", "基础化工", "钢铁", "有色金属", "电子",
	"家用电器", "食品饮料", "纺织服饰", "轻工制造", "医药生物", "公用事业", "交通运输",
	"房地产", "商贸零售", "社会服务", "银行", "非银金融", "证券", "保险", "综合",
	"建筑材料", "建筑装饰", "电力设备", "国防军工", "计算机", "传媒", "通信",
	"汽车", "机械设备", "环保", "美容护理", "电力", "航空机场", "港口航运",
	// 热门概念
	"人工智能", "半导体", "芯片", "光伏", "储能", "锂电池", "新能源", "新能源汽车",
	"机器人", "低空经济", "算力", "数据中心", "云计算", "大数据", "信创", "网络安全",
	"消费电子", "白酒", "券商", "地产", "基建", "军工", "黄金", "稀土", "氢能",
	"风电", "核电", "创新药", "中药", "医疗器械", "医疗服务", "游戏", "影视",
	"跨境电商", "免税", "旅游", "养殖", "种业", "卫星互联网", "商业航天", "智能驾驶",
	"数字货币", "金融科技", "工业母机", "3C设备", "面板", "存储",
}

func init() {
	for _, s := range newsSectorList {
		newsSectorWhitelist[s] = true
	}
}

// sentimentRule 关键词规则（P4/P5 与降级路径）：标题/摘要命中 keywords 之一即套用。
// 顺序即优先级，先命中先得。
type sentimentRule struct {
	keywords  []string
	sectors   []string
	sentiment string  // positive / negative / neutral
	score     float64 // -1 ~ 1
}

var sentimentRules = []sentimentRule{
	// 负面优先（正负词同现时按负面处理，风险提示优先）
	{[]string{"立案", "调查", "处罚", "警示函", "违规", "违法"}, nil, "negative", -0.7},
	{[]string{"退市", "*ST", "终止上市", "暂停上市"}, nil, "negative", -0.8},
	{[]string{"减持", "清仓", "质押爆仓", "平仓"}, nil, "negative", -0.5},
	{[]string{"业绩预亏", "预亏", "亏损扩大", "商誉减值", "计提减值"}, nil, "negative", -0.6},
	{[]string{"下调评级", "跌停", "闪崩", "暴跌"}, nil, "negative", -0.5},
	{[]string{"诉讼", "仲裁", "冻结", "失信"}, nil, "negative", -0.4},
	{[]string{"降价", "价格战"}, nil, "negative", -0.3},
	{[]string{"辞职", "离职", "被拘", "留置"}, nil, "negative", -0.3},
	// 正面
	{[]string{"业绩预增", "预增", "净利增长", "扭亏", "超预期"}, nil, "positive", 0.6},
	{[]string{"中标", "签订合同", "签署合同", "订单", "框架协议"}, nil, "positive", 0.5},
	{[]string{"回购", "增持"}, nil, "positive", 0.5},
	{[]string{"降准", "降息", "LPR下调"}, []string{"银行", "证券", "地产"}, "positive", 0.7},
	{[]string{"涨价", "提价", "供不应求", "涨停"}, nil, "positive", 0.4},
	{[]string{"获批", "批准", "注册证", "临床", "药监局"}, []string{"医药生物", "创新药"}, "positive", 0.5},
	{[]string{"重组", "并购", "收购", "借壳"}, nil, "positive", 0.4},
	{[]string{"分红", "派息", "特别分红"}, nil, "positive", 0.3},
	{[]string{"战略合作", "合作协议"}, nil, "positive", 0.3},
	// 板块映射为主（情绪中性偏正的产业催化）
	{[]string{"人工智能", "大模型", "AI"}, []string{"人工智能", "算力", "计算机"}, "positive", 0.3},
	{[]string{"半导体", "芯片", "晶圆", "光刻"}, []string{"半导体", "芯片", "电子"}, "positive", 0.3},
	{[]string{"锂电", "动力电池", "储能"}, []string{"锂电池", "储能", "电力设备"}, "positive", 0.3},
	{[]string{"光伏", "组件", "硅料"}, []string{"光伏", "电力设备"}, "neutral", 0.1},
	{[]string{"机器人", "人形机器人"}, []string{"机器人", "机械设备"}, "positive", 0.3},
	{[]string{"低空经济", "eVTOL", "无人机"}, []string{"低空经济", "国防军工"}, "positive", 0.3},
	{[]string{"白酒", "食品安全"}, []string{"白酒", "食品饮料"}, "neutral", 0},
	{[]string{"房地产", "楼市", "限购"}, []string{"地产", "房地产"}, "neutral", 0},
}

// applySentimentRules 关键词规则增强：返回 (sentiment, score, sectors, 是否命中)。
// 未命中给中性 0（「无消息给中性」纪律）。
func applySentimentRules(title, summary string) (string, float64, []string, bool) {
	text := title + " " + summary
	for _, r := range sentimentRules {
		for _, kw := range r.keywords {
			if strings.Contains(text, kw) {
				return r.sentiment, r.score, r.sectors, true
			}
		}
	}
	return "neutral", 0, nil, false
}

// filterSectors 对照白名单过滤板块（防幻觉），去重、上限 5。
func filterSectors(in []string) []string {
	out := make([]string, 0, 5)
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || !newsSectorWhitelist[s] || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// --- LLM 增强 ---

// resolveNewsLLM 新闻增强用的系统级 LLM：首个管理员的默认配置。
// 返回 (cfg, key, adminID, error)。无管理员/无配置时报错，调用方降级规则表。
func resolveNewsLLM() (*model.LLMConfig, string, int64, error) {
	// 系统默认 LLM：管理后台指定的回退配置优先，否则首个启用管理员的默认配置。
	// 不受"LLM 回退"用户开关控制（新闻分析是系统后台任务，由 news_auto_llm 总闸管）。
	var cfg model.LLMConfig
	if err := resolveSystemFallbackConfig(&cfg); err != nil {
		return nil, "", 0, err
	}
	key, err := common.Decrypt(cfg.APIKeyCipher)
	if err != nil || strings.TrimSpace(key) == "" {
		return nil, "", 0, errors.New("系统默认 LLM 配置缺少可用 API Key")
	}
	return &cfg, key, cfg.UserID, nil
}

// newsEnhanceItem LLM 增强的单条输出。
type newsEnhanceItem struct {
	ID             int64    `json:"id"`
	Sentiment      string   `json:"sentiment"`
	SentimentScore float64  `json:"sentiment_score"`
	ImpactScope    string   `json:"impact_scope"`
	RelatedSectors []string `json:"related_sectors"`
	PolicyLevel    int      `json:"policy_level"`
}

const newsEnhanceSystem = `你是财经新闻情绪标注员。对输入的每条新闻输出结构化标注。纪律：
1. 消息权重：公告 > 政策 > 媒体报道 > 传闻；确定性越低，|sentiment_score| 越小。
2. 旧闻重提、已充分定价的消息不给高分；单纯盘面播报（涨跌数据罗列）一律中性。
3. 无明确利好利空的给 neutral、score 取 -0.1~0.1。
4. related_sectors 只能从给定板块列表中选（最多 5 个），列表外的一律不写；与板块无关就给空数组。
5. impact_scope：影响整个市场(宏观政策/指数级)=market，影响一个或几个行业=sector，只影响个别公司=stock。
6. policy_level：中央级(国务院/中央/央行/发改委国家层面)=5，部委级(各部委/证监会)=4，交易所级=3，非政策=0。
7. 每条输入新闻恰好输出一项，保持输入顺序与 id，不得遗漏、重复或增加字段；不要复述标题、摘要或判断理由。
只输出 JSON：{"items":[{"id":新闻id,"sentiment":"positive|negative|neutral","sentiment_score":-1~1的小数,"impact_scope":"market|sector|stock","related_sectors":["..."],"policy_level":0|3|4|5}]}，不要任何解释或代码块标记。`

// enhanceBatchLLM 一批新闻走 LLM 增强（simplified=true 时只要板块与情绪，用于 P3）。
// 返回按 id 索引的结果；调用失败返回错误（调用方降级规则）。
// traceID 为本轮增强的追溯 ID（P0-2：新闻无单一业务结果行，关联只落 llm_call_logs，
// 同轮各批共享 trace、每批一个 run）。
func enhanceBatchLLM(ctx context.Context, cfg *model.LLMConfig, apiKey string, allowPrivate bool, rows []model.News, adminID int64, traceID string) (map[int64]newsEnhanceItem, error) {
	type inRow struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
		Text  string `json:"text,omitempty"`
	}
	input := make([]inRow, 0, len(rows))
	for _, n := range rows {
		txt := n.Summary
		if txt == "" {
			txt = truncateRunes(n.Content, 300)
		}
		if txt == n.Title {
			txt = ""
		}
		input = append(input, inRow{ID: n.ID, Title: n.Title, Text: truncateRunes(txt, 300)})
	}
	inJSON, _ := json.Marshal(input)
	user := "【可选板块列表】：" + strings.Join(newsSectorList, "、") +
		"\n\n【待标注新闻】（JSON 数组）：\n" + string(inJSON)

	// P0-2 修复批：run 的 prompt 版本接既有 sentiPromptVersion——newsEnhanceSystem
	// 是固定内联提示词，改措辞须递增该版本；此前传空串导致 news 调用的审计/关联缺版本归因。
	run := newLLMRun(traceID, "", "news", "news_enhance.v1", sentiPromptVersion)
	messages := []chatMessage{
		{Role: "system", Content: newsEnhanceSystem},
		{Role: "user", Content: user},
	}
	run.hashData(string(inJSON))
	run.hashPrompt(messages)
	res, err := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
		ReasoningEffort: cfg.ReasoningEffort,
		Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("news", cfg.MaxTokens),
		Messages: messages,
		JSONMode: true, AllowPrivate: allowPrivate,
		Meta: run.chatMeta(adminID, cfg, 1),
	})
	run.record(res, err)
	if err != nil {
		return nil, err
	}
	if res.Usage.TotalTokens > 0 {
		consumeQuota(adminID, res.Usage.TotalTokens, false) // 后台任务：只记 token 审计，不扣次数
	}
	var out struct {
		Items []newsEnhanceItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(res.Content)), &out); err != nil {
		// P0-9：输出不合法进统一机读码（无 repair，调用方降级规则路径）。
		return nil, refusalErrf(RefusalLLMOutputInvalid, "增强输出解析失败: %v", err)
	}
	byID := make(map[int64]newsEnhanceItem, len(out.Items))
	for _, it := range out.Items {
		byID[it.ID] = it
	}
	return byID, nil
}

// normalizeEnhance 规整 LLM 输出：情绪枚举归一、分数钳制、板块白名单过滤、scope/level 校验。
func normalizeEnhance(it newsEnhanceItem) newsEnhanceItem {
	switch strings.ToLower(strings.TrimSpace(it.Sentiment)) {
	case "positive", "利好":
		it.Sentiment = "positive"
	case "negative", "利空":
		it.Sentiment = "negative"
	default:
		it.Sentiment = "neutral"
	}
	if it.SentimentScore > 1 {
		it.SentimentScore = 1
	}
	if it.SentimentScore < -1 {
		it.SentimentScore = -1
	}
	// 情绪方向与分数矛盾时以方向为准、分数压回中性带。
	if (it.Sentiment == "positive" && it.SentimentScore < 0) ||
		(it.Sentiment == "negative" && it.SentimentScore > 0) {
		it.SentimentScore = 0
	}
	switch strings.ToLower(strings.TrimSpace(it.ImpactScope)) {
	case "market", "sector", "stock":
		it.ImpactScope = strings.ToLower(strings.TrimSpace(it.ImpactScope))
	default:
		it.ImpactScope = ""
	}
	if it.PolicyLevel != 3 && it.PolicyLevel != 4 && it.PolicyLevel != 5 {
		it.PolicyLevel = 0
	}
	it.RelatedSectors = filterSectors(it.RelatedSectors)
	return it
}

// persistEnhance 落库一条增强结果（幂等：只更新增强字段）。
func persistEnhance(id int64, sentiment string, score float64, sectors []string, scope string, level int) {
	sectorsJSON := ""
	if len(sectors) > 0 {
		if b, err := json.Marshal(sectors); err == nil {
			sectorsJSON = string(b)
		}
	}
	if err := common.DB.Model(&model.News{}).Where("id = ?", id).Updates(map[string]any{
		"sentiment": sentiment, "sentiment_score": round2(score),
		"related_sectors": sectorsJSON, "impact_scope": scope, "policy_level": level,
	}).Error; err != nil {
		common.SysWarn("新闻情绪增强落库失败(id=%d): %v", id, err)
	}
}

// EnhanceNewsRound 一轮情绪增强：挑近 48h 未增强的新闻（Sentiment 为空），
// 按来源优先级分流 LLM / 规则。挂新闻采集定时器之后调用。
func (s *NewsService) EnhanceNewsRound(ctx context.Context) {
	if common.DB == nil {
		return
	}
	var rows []model.News
	if err := common.DB.
		Select("id, title, summary, content, source_priority, related_symbols").
		Where("sentiment = '' AND publish_time > ?", time.Now().Add(-newsAIWindow)).
		Order("source_priority ASC, id ASC").Limit(newsAIRoundCap).Find(&rows).Error; err != nil || len(rows) == 0 {
		return
	}

	cfg, apiKey, adminID, llmErr := resolveNewsLLM()
	// 管理后台总闸：关闭自动 LLM 时等价于"LLM 不可用"，走既有的纯规则降级通路。
	if llmErr == nil && !setting.NewsAutoLLM() {
		llmErr = errors.New("已关闭自动 LLM 新闻分析")
	}
	allowPrivate := llmErr == nil && isAdminUser(adminID)

	// 分流：P1/P2 全量 LLM；P3 规则先行、缺板块的进简化 LLM 队列；P4/P5 纯规则。
	var llmQueue []model.News
	simplifiedUsed := 0
	for _, n := range rows {
		switch {
		case n.SourcePriority <= 2 && llmErr == nil:
			llmQueue = append(llmQueue, n)
		case n.SourcePriority == 3:
			senti, score, sectors, _ := applySentimentRules(n.Title, n.Summary)
			if len(sectors) == 0 && llmErr == nil && simplifiedUsed < newsAISimplifiedCap {
				simplifiedUsed++
				llmQueue = append(llmQueue, n)
				continue
			}
			persistEnhance(n.ID, senti, score, sectors, "stock", 0)
			atomic.AddInt64(&newsAIEnhanced, 1)
		default:
			senti, score, sectors, _ := applySentimentRules(n.Title, n.Summary)
			persistEnhance(n.ID, senti, score, sectors, "", 0)
			atomic.AddInt64(&newsAIEnhanced, 1)
		}
	}

	// LLM 队列分批调用；单批失败降级规则（行照样标记，不留重试尾巴）。
	// P0-2：同一增强轮共享一个 trace（每批一个 run），管理端按 trace 可看整轮调用。
	roundTrace := newLLMTraceID()
	for i := 0; i < len(llmQueue); i += newsAIBatchSize {
		end := i + newsAIBatchSize
		if end > len(llmQueue) {
			end = len(llmQueue)
		}
		batch := llmQueue[i:end]
		results, err := enhanceBatchLLM(ctx, cfg, apiKey, allowPrivate, batch, adminID, roundTrace)
		for _, n := range batch {
			if err == nil {
				if it, ok := results[n.ID]; ok {
					it = normalizeEnhance(it)
					persistEnhance(n.ID, it.Sentiment, it.SentimentScore, it.RelatedSectors, it.ImpactScope, it.PolicyLevel)
					atomic.AddInt64(&newsAILLMCalls, 1)
					atomic.AddInt64(&newsAIEnhanced, 1)
					continue
				}
			}
			// LLM 失败或漏了这条：规则兜底。
			senti, score, sectors, _ := applySentimentRules(n.Title, n.Summary)
			persistEnhance(n.ID, senti, score, sectors, "", 0)
			atomic.AddInt64(&newsAIEnhanced, 1)
		}
		if err != nil {
			common.SysWarn("新闻情绪增强 LLM 批次失败，本批已降级规则: %v", err)
		}
	}

	total, llm := atomic.LoadInt64(&newsAIEnhanced), atomic.LoadInt64(&newsAILLMCalls)
	if total > 0 {
		common.SysLog("新闻情绪增强: 本轮 %d 条；累计 LLM 比例 %d/%d (%.0f%%)",
			len(rows), llm, total, float64(llm)/float64(total)*100)
	}
}

// --- 个股当日聚合情绪分 ---

// sentiAggMu/sentiAggInflight (symbol,date) 并发合并：同 key 只算一次，其余等待复用
// （mutex+map 写的 singleflight 等价实现，项目无 x/sync 依赖）。
var (
	sentiAggMu       sync.Mutex
	sentiAggInflight = map[string]*sync.WaitGroup{}
)

// stockDailySentiment 个股当日聚合情绪分：优先读 stock_sentiments 缓存，
// 缺失或当日缓存超过新闻采集间隔时，由已增强新闻重新合成并 upsert。
// 权重 = 4 - source_priority（P1=3 / P2=2 / P3=1），取最新 sentiAggMaxNews 条。
// 返回 (score -1~1, 参与条数, 是否有数据)。
func stockDailySentiment(symbol, date string) (float64, int, bool) {
	return stockDailySentimentAt(symbol, date, time.Now())
}

// sentimentAggregateCacheFresh 历史日结果冻结；当日结果按新闻采集间隔刷新，避免
// 早盘首次非空聚合把午后新利空挡到第二天。row.UpdatedAt 晚于 as-of 时也不得复用，
// 供固定时钟测试/历史回放保持 point-in-time。
func sentimentAggregateCacheFresh(row *model.StockSentiment, date string, now time.Time) bool {
	if row == nil || row.UpdatedAt.IsZero() || row.UpdatedAt.After(now) {
		return false
	}
	today := now.In(time.Local).Format("2006-01-02")
	if date < today {
		return true
	}
	if date > today {
		return false
	}
	ttl := time.Duration(setting.NewsCollectIntervalMin()) * time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return now.Sub(row.UpdatedAt) < ttl
}

// stockDailySentimentAt 与 stockDailySentiment 相同，但固定本轮聚合的查询上界。
// 生产入口传 time.Now；测试用固定时钟，避免把新闻时间和严格 `< now` 上界放在
// 同一个 SQLite 时间精度边界上造成偶发漏行。
func stockDailySentimentAt(symbol, date string, now time.Time) (float64, int, bool) {
	if common.DB == nil || symbol == "" {
		return 0, 0, false
	}
	key := symbol + ":" + date

	for {
		var row model.StockSentiment
		if err := common.DB.Where("symbol = ? AND date = ?", symbol, date).First(&row).Error; err == nil &&
			sentimentAggregateCacheFresh(&row, date, now) {
			return row.Score, row.NewsCount, row.NewsCount > 0
		}
		sentiAggMu.Lock()
		if wg, ok := sentiAggInflight[key]; ok {
			sentiAggMu.Unlock()
			wg.Wait() // 别人在算，等它落库后重查
			continue
		}
		wg := &sync.WaitGroup{}
		wg.Add(1)
		sentiAggInflight[key] = wg
		sentiAggMu.Unlock()

		score, count, detail := computeDailySentimentAt(symbol, date, now)
		// P1 水位修复：仅在算到新闻（count>0）时才落缓存行——早晨新闻尚未采集时算出的
		// 「0 条」若落库，(symbol,date) 幂等会把「无新闻」冻结成全天结论，之后采集到的
		// 新闻永远进不了当日情绪。空结果不落库、每次现算（轻查询），新闻到位自然生效。
		// 非空结果按采集间隔 upsert；每个推荐批次仍把实际结果固化进候选快照。
		if count > 0 {
			row = model.StockSentiment{Symbol: symbol, Date: date, Score: score, NewsCount: count, DetailJSON: detail}
			common.DB.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "symbol"}, {Name: "date"}},
				DoUpdates: clause.AssignmentColumns([]string{"score", "news_count", "detail_json", "updated_at"}),
			}).Create(&row)
		}

		sentiAggMu.Lock()
		delete(sentiAggInflight, key)
		sentiAggMu.Unlock()
		wg.Done()
		return score, count, count > 0
	}
}

// computeDailySentiment 从当日已增强新闻合成个股情绪分（纯读，可测）。
// 返回 (score, 条数, 参与明细 JSON——{id,title,score,w} 数组，落库可复核)。
func computeDailySentiment(symbol, date string) (float64, int, string) {
	return computeDailySentimentAt(symbol, date, time.Now())
}

func computeDailySentimentAt(symbol, date string, now time.Time) (float64, int, string) {
	dayStart, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, 0, ""
	}
	dayEnd := dayStart.Add(24 * time.Hour)
	if now.Before(dayEnd) {
		dayEnd = now
	}
	if !dayEnd.After(dayStart) {
		return 0, 0, ""
	}
	var rows []model.News
	common.DB.Select("id, title, sentiment_score, source_priority").
		Where("related_symbols LIKE ? AND sentiment <> '' AND publish_time >= ? AND publish_time < ?",
			"%\""+symbol+"\"%", dayStart, dayEnd).
		Order("publish_time DESC").Limit(sentiAggMaxNews).Find(&rows)
	if len(rows) == 0 {
		return 0, 0, ""
	}
	type detailRow struct {
		ID    int64   `json:"id"`
		Title string  `json:"title"`
		Score float64 `json:"score"`
		W     float64 `json:"w"`
	}
	details := make([]detailRow, 0, len(rows))
	var sum, wsum float64
	for _, n := range rows {
		w := float64(4 - n.SourcePriority)
		if w < 1 {
			w = 1
		}
		sum += n.SentimentScore * w
		wsum += w
		details = append(details, detailRow{ID: n.ID, Title: truncateRunes(n.Title, 80), Score: n.SentimentScore, W: w})
	}
	if wsum == 0 {
		return 0, 0, ""
	}
	score := sum / wsum
	if score > 1 {
		score = 1
	}
	if score < -1 {
		score = -1
	}
	b, _ := json.Marshal(details)
	return round2(score), len(rows), string(b)
}

// --- 供分析/问答/日报消费的查询辅助 ---

// newsBrief 注入 AI 上下文的新闻精简行。P1-4 起带来源优先级与影响范围（蓝图 F：
// 注入项保留 publish_time/source/priority 元数据，窗口/来源对齐由程序声明）。
type newsBrief struct {
	Title     string `json:"title"`
	Sentiment string `json:"sentiment,omitempty"` // 利好/利空/中性（中文标签，供模型直接引用）
	Time      string `json:"time"`                // 值取 publish_time（业务发布时点，非采集时刻）
	Source    string `json:"source"`
	// Category 新闻类别（telegraph/flash/stock；蓝图 F：注入项保留 category 元数据）。
	Category string `json:"category,omitempty"`
	// Priority 来源优先级（1=财联社电报级 … 5=低优先级；0=未知）。P1/P2 全量 LLM 增强、
	// P3 规则先行的成本分层同一口径（EnhanceNewsRound）。
	Priority int `json:"priority,omitempty"`
	// Scope 影响范围（market/sector/stock；空=未增强或未判定）——LLM 增强产物透传。
	Scope string `json:"scope,omitempty"`
}

func sentimentCN(s string) string {
	switch s {
	case "positive":
		return "利好"
	case "negative":
		return "利空"
	case "neutral":
		return "中性"
	}
	return ""
}

// newsBriefMaxAge 注入 AI 上下文的新闻最大时龄（P0-7 news 链路 stale fail-closed）：
// 「最近 N 条」无时间下限会让数月前的旧闻冒充近期消息面（Time 无年份更无从辨别），
// 7 天覆盖「近期消息面」语义；窗口内无新闻时 news 块如实缺席（分析/问答 prompt 已有
// 无新闻的处理口径），不许放宽窗口硬凑条数。
const newsBriefMaxAge = 7 * 24 * time.Hour

// latestNewsBriefsAt 某标的近 7 天内最新 limit 条新闻（标题+情绪标签+来源元数据），
// P1-4 起经 latestNewsWindow 消费（briefs 与窗口声明同刻构建）。查询失败以 error 区分
// 「确无」与「没查到」（审查修复批：不再吞错——window_meta.source_query_status 消费）。
func latestNewsBriefsAt(symbol string, limit int, now time.Time) ([]newsBrief, error) {
	if common.DB == nil || len(symbol) != 6 {
		return nil, nil
	}
	var rows []model.News
	if err := common.DB.Select("title, sentiment, publish_time, source, category, source_priority, impact_scope").
		Where("related_symbols LIKE ? AND publish_time >= ? AND publish_time <= ?", "%\""+symbol+"\"%", now.Add(-newsBriefMaxAge), now).
		Order("publish_time DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]newsBrief, 0, len(rows))
	for _, n := range rows {
		out = append(out, newsBrief{
			Title: n.Title, Sentiment: sentimentCN(n.Sentiment),
			// 带年份的完整时点（旧格式 "01-02 15:04" 跨年会误导模型把去年当今年）。
			Time: n.PublishTime.Format("2006-01-02 15:04"), Source: n.Source,
			Category: n.Category, Priority: n.SourcePriority, Scope: n.ImpactScope,
		})
	}
	return out, nil
}

// --- P1-4 新闻窗口与来源对齐（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.2 P1-4）---

// newsWindowVersion 窗口/对齐算法版本：改分类阈值或规则必须递增（source_alignment 是
// 程序按版本化阈值计算的声明，模型只解释分歧、不得重新分类——蓝图 F 纪律）。
// nw2（审查修复批）：①对齐/覆盖统计基于**完整 7 日窗口**（上限 newsWindowStatMax 条）
// 而非仅注入的最新 5 条；②single_source 按**独立来源数**判定（同一媒体多条同向新闻
// 不再冒充多源一致）；③全部无情绪标注（未增强）时 alignment=unavailable（无从判定，
// 不再误标 aligned）；④新增 total_in_window/injected_count/source_query_status——
// 查询失败与「窗口内确无」显式分开。nw1：注入条目口径、主导比 0.7。
const newsWindowVersion = "nw2"

// newsAlignmentDominantRatio 对齐分类阈值：正负两方向并存时，主导方向占
// 方向性样本的比例 ≥ 该值为 mixed（主导但有杂音），否则 divergent（显著分歧）。
const newsAlignmentDominantRatio = 0.7

// newsWindowStatMax 窗口统计的取样上限（防极端标的整窗数千条拖垮快照构建；
// 达到上限时统计代表「窗口内最新 200 条」，total_in_window 仍是全窗口真实计数）。
const newsWindowStatMax = 200

// source_alignment 封闭枚举（蓝图 F）。
const (
	newsAlignAligned      = "aligned"       // 多源且方向一致（含全中性）
	newsAlignMixed        = "mixed"         // 双向并存但一方主导（≥70%）
	newsAlignDivergent    = "divergent"     // 双向显著分歧（主导 <70%）
	newsAlignSingleSource = "single_source" // 独立来源 ≤1，无从交叉验证（不得写成「市场共识」）
	newsAlignUnavailable  = "unavailable"   // 窗口内无新闻，或全部条目无情绪标注/查询失败（无从判定）
)

// newsWindowStat 窗口统计行（对齐/覆盖的计算输入——完整窗口口径，非仅注入条目）。
type newsWindowStat struct {
	Sentiment string
	Source    string
	Priority  int
}

// newsWindowMeta 注入快照的机读窗口声明（L2 数据段，非 prompt 文本）：把既有
// [now-7d, now] 硬窗口显式化为机器可读契约——窗口外新闻 0 注入由查询双边界保证，
// 这里声明「注入的就是这个窗口、来源覆盖如此、方向对齐程度如此」。
type newsWindowMeta struct {
	WindowStart string `json:"window_start"` // 窗口下界（含），本地时区
	WindowEnd   string `json:"window_end"`   // 窗口上界（含）=构建时刻——未来记录恒不注入
	// TotalInWindow 窗口内新闻总条数（完整窗口计数）；InjectedCount 实际注入条数
	//（≤5 条最新）。两者分开声明「统计基于全窗口、正文只注入最新若干条」。
	TotalInWindow int `json:"total_in_window"`
	InjectedCount int `json:"injected_count"`
	// SourceQueryStatus 查询状态（ok/failed）：failed=来源查询未完成，alignment 为
	// unavailable 时消费方可区分「查了确无」与「没查成」（P1-4 契约：只有查询完整
	// 且窗口内确无实质事件才可当 no_material_event）。
	SourceQueryStatus string `json:"source_query_status"`
	// SourceCoverage 来源优先级覆盖计数（键 "P1"…"P5"，"P0"=未知优先级）：完整窗口
	// 口径（nw2）。空 map 省略（无新闻）。
	SourceCoverage map[string]int `json:"source_coverage,omitempty"`
	// SourceAlignment 程序化方向对齐分类（版本化阈值，见 newsAlignmentDominantRatio）。
	// 模型只能解释分歧，不得重新分类；单源不得写成共识。
	SourceAlignment string `json:"source_alignment"`
	Version         string `json:"version"`
}

// buildNewsWindowMeta 由窗口统计组装窗口声明（纯函数，可测）。stats 为完整窗口统计行
// （nw2），total 为窗口内真实总数，injected 为实际注入条数；queryOK=false 时如实声明
// 查询失败（alignment=unavailable，不冒充「确无」）。
func buildNewsWindowMeta(stats []newsWindowStat, total, injected int, queryOK bool, now time.Time) newsWindowMeta {
	m := newsWindowMeta{
		WindowStart:       now.Add(-newsBriefMaxAge).Format("2006-01-02 15:04"),
		WindowEnd:         now.Format("2006-01-02 15:04"),
		TotalInWindow:     total,
		InjectedCount:     injected,
		SourceQueryStatus: "ok",
		SourceAlignment:   computeSourceAlignment(stats),
		Version:           newsWindowVersion,
	}
	if !queryOK {
		m.SourceQueryStatus = "failed"
		m.SourceAlignment = newsAlignUnavailable
		return m
	}
	if len(stats) > 0 {
		cov := make(map[string]int, 4)
		for _, s := range stats {
			cov[fmt.Sprintf("P%d", s.Priority)]++
		}
		m.SourceCoverage = cov
	}
	return m
}

// computeSourceAlignment 方向对齐分类（nw2，完整窗口口径）：
//   - 0 条=unavailable；
//   - 独立来源 ≤1（无论条数）=single_source——同一媒体连发多条同向新闻不构成交叉验证；
//   - 无任何方向样本：全部显式中性=aligned（多源一致中性），否则（含未增强/无标注条目
//     且无一显式判向）=unavailable——「没标注」不是「一致」；
//   - 双向并存按主导占比分 mixed/divergent，单方向=aligned。
func computeSourceAlignment(stats []newsWindowStat) string {
	if len(stats) == 0 {
		return newsAlignUnavailable
	}
	srcs := map[string]bool{}
	pos, neg, neutral := 0, 0, 0
	for _, s := range stats {
		srcs[strings.TrimSpace(s.Source)] = true
		switch s.Sentiment {
		case "positive":
			pos++
		case "negative":
			neg++
		case "neutral":
			neutral++
		}
	}
	if len(srcs) <= 1 {
		return newsAlignSingleSource
	}
	if pos == 0 && neg == 0 {
		if neutral > 0 {
			return newsAlignAligned
		}
		return newsAlignUnavailable
	}
	if pos == 0 || neg == 0 {
		return newsAlignAligned
	}
	dominant := pos
	if neg > dominant {
		dominant = neg
	}
	if float64(dominant)/float64(pos+neg) >= newsAlignmentDominantRatio {
		return newsAlignMixed
	}
	return newsAlignDivergent
}

// loadNewsWindowStats 完整窗口统计行 + 总数（nw2）：统计与注入分开查——注入限 5 条控
// prompt 预算，统计覆盖全窗口（取样上限 newsWindowStatMax）才能代表「7 日窗口的来源
// 覆盖与方向对齐」。
func loadNewsWindowStats(symbol string, now time.Time) ([]newsWindowStat, int, error) {
	if common.DB == nil || len(symbol) != 6 {
		return nil, 0, nil
	}
	q := common.DB.Model(&model.News{}).
		Where("related_symbols LIKE ? AND publish_time >= ? AND publish_time <= ?",
			"%\""+symbol+"\"%", now.Add(-newsBriefMaxAge), now)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.News
	if err := q.Select("sentiment, source, source_priority").
		Order("publish_time DESC").Limit(newsWindowStatMax).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	stats := make([]newsWindowStat, 0, len(rows))
	for _, n := range rows {
		stats = append(stats, newsWindowStat{Sentiment: n.Sentiment, Source: n.Source, Priority: n.SourcePriority})
	}
	return stats, int(total), nil
}

// latestNewsWindow 取窗口内新闻 + 机读窗口声明（P1-4 快照注入入口）：调用方拿到的
// briefs 与 meta 基于同一时刻构建，窗口声明与实际注入内容不会错位。
func latestNewsWindow(symbol string, limit int) ([]newsBrief, newsWindowMeta) {
	return latestNewsWindowAt(symbol, limit, time.Now())
}

func latestNewsWindowAt(symbol string, limit int, now time.Time) ([]newsBrief, newsWindowMeta) {
	briefs, berr := latestNewsBriefsAt(symbol, limit, now)
	stats, total, serr := loadNewsWindowStats(symbol, now)
	queryOK := berr == nil && serr == nil
	if !queryOK {
		common.SysWarn("新闻窗口查询失败 %s: brief=%v stats=%v", symbol, berr, serr)
	}
	return briefs, buildNewsWindowMeta(stats, total, len(briefs), queryOK, now)
}

// newsTitleTexts 从个股快照的 news 块提取标题等文本（信任层：标题里的小数是
// 「文本型合法来源」，须并入证据核验值域，否则模型忠实引用会被误报幻觉）。
func newsTitleTexts(snapshot map[string]any) []string {
	blk, ok := snapshot["news"].(map[string]any)
	if !ok {
		return nil
	}
	items, ok := blk["items"].([]newsBrief)
	if ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, it.Title)
		}
		return out
	}
	// 快照经 JSON 反序列化后（问答复用落库快照）结构是 []any/map[string]any。
	arr, ok := blk["items"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			if t, ok := m["title"].(string); ok {
				out = append(out, t)
			}
		}
	}
	return out
}
