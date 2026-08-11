package service

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

const maxDiscoveryPoolIntake = 120

type CandidateDiscoveryDay struct {
	TradeDate   string   `json:"trade_date"`
	Channels    []string `json:"channels"`
	BestRank    int      `json:"best_rank"`
	BestScore   float64  `json:"best_score"`
	RankChange  int      `json:"rank_change"`
	ScoreChange float64  `json:"score_change"`
}

// CandidateDiscoverySummary 是进入 LLM 的紧凑候选记忆，最多 5 个交易日。
type CandidateDiscoverySummary struct {
	FirstSeenDate   string                  `json:"first_seen_date"`
	LastSeenDate    string                  `json:"last_seen_date"`
	SeenDays5D      int                     `json:"seen_days_5d"`
	ConsecutiveDays int                     `json:"consecutive_days"`
	Days            []CandidateDiscoveryDay `json:"days"`
	DataStatus      string                  `json:"data_status"`
	PartialReason   string                  `json:"partial_reason,omitempty"`
}

type CandidateNewsBrief struct {
	Title          string  `json:"title"`
	Source         string  `json:"source"`
	PublishTime    string  `json:"publish_time"`
	Sentiment      string  `json:"sentiment,omitempty"`
	SentimentScore float64 `json:"sentiment_score,omitempty"`
}

type discoveryPoolCandidate struct {
	candidate candidate
	source    string
	channel   string
	bestRank  int
}

type discoveryAggregate struct {
	name     string
	market   string
	first    string
	last     string
	status   string
	reason   string
	byDate   map[string][]model.CandidateDiscoveryItem
	bestRank int
	channel  string
}

// recentDiscoveryCandidates 只读已落库事实，不调用宽表扫描。历史 score 仅进入摘要，
// 返回 candidate.Score 保持零值，后续由 scorePool 基于当前行情重算。
func recentDiscoveryCandidates(market string, limit int) []discoveryPoolCandidate {
	if common.DB == nil || market != "cn" {
		return nil
	}
	if limit <= 0 || limit > maxDiscoveryPoolIntake {
		limit = maxDiscoveryPoolIntake
	}
	var latest model.CandidateDiscoveryRun
	if err := common.DB.Where("market = ? AND owner_type = ? AND status IN ?", market, model.JobOwnerSystem, []string{DiscoveryRunStatusOK, DiscoveryRunStatusPart}).Order("trade_date DESC, id DESC").First(&latest).Error; err != nil {
		return nil
	}
	dates := recentOpenDates(market, latest.TradeDate, 5)
	if len(dates) == 0 || dates[len(dates)-1] != latest.TradeDate {
		dates = append(dates, latest.TradeDate)
	}
	if len(dates) > 5 {
		dates = dates[len(dates)-5:]
	}
	var rows []model.CandidateDiscoveryItem
	query := common.DB.Joins("JOIN candidate_discovery_runs AS discovery_run ON discovery_run.id = candidate_discovery_items.run_id").
		Where("candidate_discovery_items.market = ? AND candidate_discovery_items.trade_date IN ? AND candidate_discovery_items.discovery_version = ? AND candidate_discovery_items.data_status IN ?", market, dates, latest.DiscoveryVersion, []string{DiscoveryItemReady, DiscoveryItemPartial}).
		Where("discovery_run.owner_type = ? AND discovery_run.status IN ? AND discovery_run.parameter_hash = ?", model.JobOwnerSystem, []string{DiscoveryRunStatusOK, DiscoveryRunStatusPart}, latest.ParameterHash)
	if err := query.Order("candidate_discovery_items.trade_date DESC, candidate_discovery_items.channel ASC, candidate_discovery_items.rank ASC, candidate_discovery_items.symbol ASC").Find(&rows).Error; err != nil {
		return nil
	}
	aggs := map[string]*discoveryAggregate{}
	byDateChannel := map[string]map[string][]model.CandidateDiscoveryItem{}
	for _, row := range rows {
		a := aggs[row.Symbol]
		if a == nil {
			a = &discoveryAggregate{market: row.Market, name: row.Name, first: row.FirstSeenDate, last: row.TradeDate, status: row.DataStatus, reason: row.PartialReason, byDate: map[string][]model.CandidateDiscoveryItem{}, bestRank: row.Rank, channel: row.Channel}
			aggs[row.Symbol] = a
		}
		if a.first == "" || (row.FirstSeenDate != "" && row.FirstSeenDate < a.first) {
			a.first = row.FirstSeenDate
		}
		if row.TradeDate > a.last {
			a.last = row.TradeDate
		}
		if row.Rank < a.bestRank || a.bestRank == 0 {
			a.bestRank, a.channel = row.Rank, row.Channel
		}
		if row.DataStatus != DiscoveryItemReady {
			a.status, a.reason = row.DataStatus, row.PartialReason
		}
		a.byDate[row.TradeDate] = append(a.byDate[row.TradeDate], row)
		if byDateChannel[row.TradeDate] == nil {
			byDateChannel[row.TradeDate] = map[string][]model.CandidateDiscoveryItem{}
		}
		byDateChannel[row.TradeDate][row.Channel] = append(byDateChannel[row.TradeDate][row.Channel], row)
	}
	// 每个交易日按通道轮转，保证推荐建池不会被单一票型占满。
	order := make([]string, 0, limit)
	seen := map[string]bool{}
	for di := len(dates) - 1; di >= 0 && len(order) < limit; di-- {
		groups := byDateChannel[dates[di]]
		for rank := 0; ; rank++ {
			added := false
			for _, channel := range discoveryChannels {
				items := groups[channel]
				if rank >= len(items) {
					continue
				}
				added = true
				if !seen[items[rank].Symbol] {
					seen[items[rank].Symbol] = true
					order = append(order, items[rank].Symbol)
					if len(order) >= limit {
						break
					}
				}
			}
			if !added || len(order) >= limit {
				break
			}
		}
	}
	out := make([]discoveryPoolCandidate, 0, len(order))
	for _, symbol := range order {
		a := aggs[symbol]
		if a == nil {
			continue
		}
		summary := &CandidateDiscoverySummary{FirstSeenDate: a.first, LastSeenDate: a.last, DataStatus: a.status, PartialReason: a.reason}
		for _, date := range dates {
			items := a.byDate[date]
			if len(items) == 0 {
				continue
			}
			sort.SliceStable(items, func(i, j int) bool {
				if items[i].Rank != items[j].Rank {
					return items[i].Rank < items[j].Rank
				}
				return items[i].Channel < items[j].Channel
			})
			day := CandidateDiscoveryDay{TradeDate: date, BestRank: items[0].Rank, BestScore: items[0].Score, RankChange: items[0].RankChange, ScoreChange: items[0].ScoreChange}
			for _, item := range items {
				day.Channels = append(day.Channels, item.Channel)
				if item.Rank < day.BestRank {
					day.BestRank, day.BestScore, day.RankChange, day.ScoreChange = item.Rank, item.Score, item.RankChange, item.ScoreChange
				}
			}
			sort.Strings(day.Channels)
			summary.Days = append(summary.Days, day)
		}
		summary.SeenDays5D = len(summary.Days)
		latestItems := a.byDate[a.last]
		var factors map[string]float64
		if len(latestItems) > 0 {
			_ = json.Unmarshal([]byte(latestItems[0].FactorSummary), &factors)
			for _, item := range latestItems {
				if item.ConsecutiveDays > summary.ConsecutiveDays {
					summary.ConsecutiveDays = item.ConsecutiveDays
				}
			}
		}
		c := candidate{Symbol: symbol, Market: market, Name: a.name, Discovery: summary}
		if factors != nil {
			c.Price = factors["close"]
			c.Amount = factors["amount_yi"] * 1e8
			c.TurnoverRate = factors["turnover_rate"]
		}
		source := "recent_discovery"
		if a.last == latest.TradeDate {
			source = "daily_discovery"
		}
		out = append(out, discoveryPoolCandidate{candidate: c, source: source, channel: a.channel, bestRank: a.bestRank})
	}
	return out
}

// enrichCandidatePromptContext 注入标题级真实新闻。窗口、条数和字段都有硬上限，
// 不读取 Content/Summary，避免正文进入 prompt；RelatedSymbols 需精确包含当前代码。
func enrichCandidatePromptContext(cands []candidate, asOf time.Time) {
	if common.DB == nil || len(cands) == 0 {
		return
	}
	if asOf.IsZero() {
		asOf = time.Now()
	}
	from := asOf.AddDate(0, 0, -7)
	for i := range cands {
		var rows []model.News
		if err := common.DB.Select("title", "source", "publish_time", "sentiment", "sentiment_score", "related_symbols").Where("publish_time >= ? AND publish_time <= ? AND related_symbols LIKE ?", from, asOf, "%\""+cands[i].Symbol+"\"%").Order("publish_time DESC, source_priority ASC, id DESC").Limit(3).Find(&rows).Error; err != nil {
			continue
		}
		for _, row := range rows {
			var syms []string
			if json.Unmarshal([]byte(row.RelatedSymbols), &syms) != nil || !stringInSlice(syms, cands[i].Symbol) {
				continue
			}
			title := strings.TrimSpace(row.Title)
			if title == "" {
				continue
			}
			cands[i].NewsBrief = append(cands[i].NewsBrief, CandidateNewsBrief{Title: truncateRunes(title, 120), Source: truncateRunes(row.Source, 32), PublishTime: row.PublishTime.In(time.Local).Format("2006-01-02 15:04"), Sentiment: row.Sentiment, SentimentScore: round2(row.SentimentScore)})
		}
	}
}

func stringInSlice(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
