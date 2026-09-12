package service

import "strings"

type recIndustryPeers struct {
	Version       string   `json:"version"`
	Industry      string   `json:"industry"`
	PESample      int      `json:"pe_sample"`
	PEPercentile  *float64 `json:"pe_percentile,omitempty"`
	PBSample      int      `json:"pb_sample"`
	PBPercentile  *float64 `json:"pb_percentile,omitempty"`
	CapSample     int      `json:"cap_sample"`
	CapPercentile *float64 `json:"cap_percentile,omitempty"`
}

// 同一报价有效候选集合内比较行业分位，至少 5 个有效同行。它不是全行业估值分位，
// 样本数与机会集范围随快照声明；样本不足保持缺失，不拿全市场低 PE 当成行业低估。
func attachRecommendationPeers(pool []candidate, industryBy map[string]string) {
	type group struct{ pe, pb, cap []float64 }
	groups := map[string]*group{}
	eligible := func(c candidate) bool {
		return (c.Excluded == "" || strings.HasPrefix(c.Excluded, poolFullPrefix)) && c.QuoteAsOf != ""
	}
	for i := range pool {
		pool[i].IndustryPeers=nil
		pool[i].Industry = industryBy[pool[i].Symbol]
		if !eligible(pool[i]) || pool[i].Industry == "" {
			continue
		}
		g := groups[pool[i].Industry]
		if g == nil {
			g = &group{}
			groups[pool[i].Industry] = g
		}
		for _, v := range []struct {
			value float64
			dest  *[]float64
		}{{pool[i].PETTM, &g.pe}, {pool[i].PB, &g.pb}, {pool[i].TotalCap, &g.cap}} {
			if v.value > 0 && finiteRecNumber(v.value) {
				*v.dest = append(*v.dest, v.value)
			}
		}
	}
	percentile := func(values []float64, value float64) *float64 {
		if len(values) < 5 || value <= 0 || !finiteRecNumber(value) {
			return nil
		}
		less, ties := 0, 0
		for _, v := range values {
			if v < value {
				less++
			} else if v == value {
				ties++
			}
		}
		return recNumber((float64(less) + float64(ties)/2) / float64(len(values)) * 100)
	}
	for i := range pool {
		g := groups[pool[i].Industry]
		if g == nil || !eligible(pool[i]) {
			continue
		}
		pool[i].IndustryPeers = &recIndustryPeers{Version: "ip1", Industry: pool[i].Industry,
			PESample: len(g.pe), PEPercentile: percentile(g.pe, pool[i].PETTM),
			PBSample: len(g.pb), PBPercentile: percentile(g.pb, pool[i].PB),
			CapSample: len(g.cap), CapPercentile: percentile(g.cap, pool[i].TotalCap)}
	}
}
