package service

import (
	"encoding/json"
	"errors"
	"strings"

	"quantvista/model"
)

// recommendationRuntimeSnapshot 随接受请求冻结；不含 API key、行情或用户凭据。
type recommendationRuntimeSnapshot struct {
	Version         int                         `json:"version"`
	UserID          int64                       `json:"user_id"`
	RecType         string                      `json:"rec_type"`
	StrategyVersion string                      `json:"strategy_version"`
	ProfileVersion  string                      `json:"profile_version"`
	Definition      strategyTemplate            `json:"definition"`
	Guide           string                      `json:"guide"`
	Tree            *CondNode                   `json:"tree,omitempty"`
	Screen          *recommendationFrozenScreen `json:"screen,omitempty"`
	Scoring         recScoringRuntime           `json:"scoring"`
	Hash            string                      `json:"hash,omitempty"`
}

type recommendationFrozenScreen struct {
	UserID     int64     `json:"user_id"`
	Name       string    `json:"name"`
	StrategyID int64     `json:"strategy_id,omitempty"`
	RevisionID int64     `json:"revision_id,omitempty"`
	Tree       *CondNode `json:"tree"`
}

func freezeRecommendationRuntime(userID int64, recType string, strat *strategyTemplate) (*recommendationRuntimeSnapshot, error) {
	r := &recommendationRuntimeSnapshot{Version: 1, UserID: userID, RecType: recType, StrategyVersion: recStrategyVersion, ProfileVersion: recommendationProfileVersion, Definition: *strat, Guide: strat.guide, Tree: strat.tree, Scoring: strat.scoring}
	r.Definition.ScoreProfile = strat.baseKey
	r.Definition.Intent = publicStrategy(*strat).Intent
	if r.Scoring.Algorithm == "" {
		r.Scoring.Algorithm = recommendationScoringVersion
	}
	if strat.screen != nil {
		r.Screen = &recommendationFrozenScreen{UserID: userID, Name: strat.Name, StrategyID: strat.screen.strategyID, RevisionID: strat.StrategyRevisionID, Tree: strat.tree}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b) > 128<<10 {
		return nil, errors.New("完整推荐配置超过可保存范围")
	}
	// 深复制条件树和模型数组，后续目录编辑不能改动已接受的内存计划。
	var copy recommendationRuntimeSnapshot
	if err := json.Unmarshal(b, &copy); err != nil {
		return nil, err
	}
	copy.Hash = rankingJSONHash(b)
	return &copy, nil
}

func thawRecommendationRuntime(r *recommendationRuntimeSnapshot, userID int64, req RecommendRequest) (*strategyTemplate, error) {
	if r == nil || r.Version != 1 || r.UserID != userID || r.RecType != req.Type || r.Definition.Key != req.Strategy || r.Definition.StrategyRevisionID != req.StrategyRevisionID {
		return nil, errors.New("推荐任务快照与用户、策略或周期不一致")
	}
	if r.StrategyVersion != recStrategyVersion || r.ProfileVersion != recommendationProfileVersion {
		return nil, errors.New("该排队任务的策略算法版本已不受支持，请重新生成；历史结果保持原意")
	}
	copy := *r
	copy.Hash = ""
	b, err := json.Marshal(copy)
	if err != nil || rankingJSONHash(b) != r.Hash {
		return nil, errors.New("推荐任务快照摘要不一致")
	}
	if !model.ValidStrategyScoreProfile(r.Definition.ScoreProfile) || r.Definition.ScoreProfile == "" {
		return nil, errors.New("任务评分侧重无效")
	}
	if err := validateRecScoringRuntime(r.Scoring); err != nil {
		return nil, err
	}
	strat := r.Definition
	strat.baseKey, strat.guide, strat.tree, strat.scoring = strat.ScoreProfile, r.Guide, r.Tree, r.Scoring
	if r.Screen != nil {
		if r.Screen.UserID != userID || r.Screen.Name != strat.Name || r.Screen.RevisionID != strat.StrategyRevisionID {
			return nil, errors.New("冻结选股条件不属于该任务")
		}
		if _, err := canonicalCondTreeForRuntime(r.Tree); err != nil {
			return nil, err
		}
		a, _ := json.Marshal(r.Tree)
		b, _ := json.Marshal(r.Screen.Tree)
		if string(a) != string(b) {
			return nil, errors.New("选股扫描与终选条件快照不一致")
		}
		strat.screen = &recScreenBinding{strategyID: r.Screen.StrategyID, strategyRevisionID: r.Screen.RevisionID}
		strat.frozenScan = r.Screen
	} else if r.Tree != nil || strings.HasPrefix(strat.Key, recStrategyScreenPrefix) || strings.HasPrefix(strat.Key, recStrategyTemplatePrefix) {
		return nil, errors.New("任务缺少冻结选股条件")
	}
	return &strat, nil
}

func canonicalCondTreeForRuntime(tree *CondNode) (*CondNode, error) {
	copy, _, err := canonicalCondTree(tree)
	return copy, err
}
