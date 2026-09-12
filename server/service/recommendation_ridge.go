package service

import (
	"context"
	"errors"
	"math"
	"sort"
)

const rankingRidgeVersion = "ridge1"

type RankingRidgeWeight struct {
	Feature string  `json:"feature"`
	Weight  float64 `json:"weight"`
}

// 线性模型只作排序基线；输出不是获利概率。均值、尺度与缺失率全部仅由训练段拟合。
type RankingRidgeModel struct {
	Version      string               `json:"version"`
	Lambda       float64              `json:"lambda"`
	Samples      int                  `json:"samples"`
	Dates        int                  `json:"dates"`
	Intercept    float64              `json:"intercept"`
	Means        []float64            `json:"means"`
	Scales       []float64            `json:"scales"`
	MissingRates []float64            `json:"missing_rates"`
	Active       []bool               `json:"active"`
	DesignMeans  []float64            `json:"design_means"`
	Weights      []RankingRidgeWeight `json:"weights"`
}

func (m *RankingRidgeModel) rawDesign(x []float64) []float64 {
	p := len(m.Means)
	out := make([]float64, 2*p)
	for j := 0; j < p; j++ {
		missing := 1.0
		if j < len(x) && finiteRecNumber(x[j]) {
			if m.Active[j] {
				out[j] = bounded((x[j]-m.Means[j])/m.Scales[j], -8, 8)
			}
			missing = 0
		}
		scale := math.Sqrt(m.MissingRates[j] * (1 - m.MissingRates[j]))
		if scale < 1e-8 {
			scale = 1
		}
		if m.MissingRates[j] > 0 && m.MissingRates[j] < 1 {
			out[p+j] = bounded((missing-m.MissingRates[j])/scale, -8, 8)
		}
	}
	return out
}

func (m *RankingRidgeModel) design(x []float64) []float64 {
	out := m.rawDesign(x)
	for i := range out {
		out[i] -= m.DesignMeans[i]
	}
	return out
}

func (m *RankingRidgeModel) score(x []float64) float64 {
	design := m.design(x)
	score := m.Intercept
	for i, v := range design {
		score += v * m.Weights[i].Weight
	}
	return score
}

func fitRankingRidge(samples []rankingResearchSample, target string, lambda float64, contexts ...context.Context) (*RankingRidgeModel, error) {
	ctx := jobSubmissionContext(contexts...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lambda <= 0 || !finiteRecNumber(lambda) {
		return nil, errors.New("正则参数必须为正数")
	}
	var train []rankingResearchSample
	perDate := map[string]int{}
	for _, s := range samples {
		if _, ok := rankingSampleTarget(s, target); ok {
			train = append(train, s)
			perDate[s.Date]++
		}
	}
	if len(train) < 100 || len(perDate) < 20 {
		return nil, errors.New("训练至少需要 100 个成熟样本且覆盖 20 个不同日期")
	}
	sort.SliceStable(train, func(i, j int) bool { return researchSampleLess(train[i], train[j]) })
	p := len(rankingFeatureNames)
	m := &RankingRidgeModel{Version: rankingRidgeVersion, Lambda: lambda, Samples: len(train), Dates: len(perDate), Means: make([]float64, p), Scales: make([]float64, p), MissingRates: make([]float64, p), Active: make([]bool, p), DesignMeans: make([]float64, 2*p), Weights: make([]RankingRidgeWeight, 2*p)}
	knownWeight := make([]float64, p)
	totalWeight := 0.0
	// 每个信号日期总权重相同，重复生成多个批次不能伪造更多独立市场样本。
	for _, s := range train {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w := 1 / float64(perDate[s.Date])
		totalWeight += w
		y, _ := rankingSampleTarget(s, target)
		m.Intercept += w * bounded(y, -50, 50)
		for j := 0; j < p; j++ {
			if j < len(s.X) && finiteRecNumber(s.X[j]) {
				m.Means[j] += w * s.X[j]
				knownWeight[j] += w
			} else {
				m.MissingRates[j] += w
			}
		}
	}
	m.Intercept /= totalWeight
	for j := 0; j < p; j++ {
		if knownWeight[j] > 0 {
			m.Means[j] /= knownWeight[j]
		}
		m.MissingRates[j] /= totalWeight
	}
	for _, s := range train {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w := 1 / float64(perDate[s.Date])
		for j := 0; j < p; j++ {
			if j < len(s.X) && finiteRecNumber(s.X[j]) {
				delta := s.X[j] - m.Means[j]
				m.Scales[j] += w * delta * delta
			}
		}
	}
	for j := 0; j < p; j++ {
		if knownWeight[j] > 0 {
			m.Scales[j] = math.Sqrt(m.Scales[j] / knownWeight[j])
		}
		if m.Scales[j] < 1e-8 {
			m.Scales[j] = 1
		} else {
			m.Active[j] = true
		}
	}
	// 裁剪与缺失指示会改变列均值；在训练段再次中心化，截距才仍等于训练目标均值。
	for _, s := range train {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		x := m.rawDesign(s.X)
		w := 1 / float64(perDate[s.Date]) / totalWeight
		for j := range x {
			m.DesignMeans[j] += w * x[j]
		}
	}
	n := 2 * p
	gram := make([][]float64, n)
	rhs := make([]float64, n)
	for i := range gram {
		gram[i] = make([]float64, n)
		gram[i][i] = lambda
	}
	for _, s := range train {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		x := m.design(s.X)
		y, _ := rankingSampleTarget(s, target)
		y = bounded(y, -50, 50) - m.Intercept
		w := 1 / float64(perDate[s.Date])
		for i := 0; i < n; i++ {
			rhs[i] += w * x[i] * y
			for j := 0; j <= i; j++ {
				gram[i][j] += w * x[i] * x[j]
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			gram[j][i] = gram[i][j]
		}
	}
	weights, err := solveRankingSPD(gram, rhs)
	if err != nil {
		return nil, err
	}
	for i, w := range weights {
		name := rankingFeatureNames[i%p]
		if i >= p {
			name = "missing:" + name
		}
		m.Weights[i] = RankingRidgeWeight{Feature: name, Weight: w}
	}
	return m, nil
}

func solveRankingSPD(a [][]float64, b []float64) ([]float64, error) {
	n := len(b)
	l := make([][]float64, n)
	for i := range l {
		l[i] = make([]float64, n)
	}
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			v := a[i][j]
			for k := 0; k < j; k++ {
				v -= l[i][k] * l[j][k]
			}
			if i == j {
				if v <= 0 || !finiteRecNumber(v) {
					return nil, errors.New("正则线性系统不可解")
				}
				l[i][j] = math.Sqrt(v)
			} else {
				l[i][j] = v / l[j][j]
			}
		}
	}
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		v := b[i]
		for j := 0; j < i; j++ {
			v -= l[i][j] * y[j]
		}
		y[i] = v / l[i][i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		v := y[i]
		for j := i + 1; j < n; j++ {
			v -= l[j][i] * x[j]
		}
		x[i] = v / l[i][i]
		if !finiteRecNumber(x[i]) {
			return nil, errors.New("线性系数无效")
		}
	}
	return x, nil
}

func rankedRidgeWeights(m *RankingRidgeModel) []RankingRidgeWeight {
	if m == nil {
		return nil
	}
	out := append([]RankingRidgeWeight(nil), m.Weights...)
	sort.SliceStable(out, func(i, j int) bool { return math.Abs(out[i].Weight) > math.Abs(out[j].Weight) })
	return out
}
