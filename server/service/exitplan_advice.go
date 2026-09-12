package service

// 只送与卖出决定有关的数字和限制，不重复发送全部初始证据、规划状态及公司行动承接快照。
type positionAdviceExitPlan struct {
	Version           string   `json:"version"`
	Profile           string   `json:"profile"`
	SourceHash        string   `json:"source_hash"`
	InitialStop       float64  `json:"initial_stop"`
	CurrentStop       float64  `json:"current_stop"`
	Target            float64  `json:"first_target"`
	ExtendedTarget    float64  `json:"extended_target"`
	Breakeven         float64  `json:"breakeven"`
	Stage             string   `json:"stage"`
	SellableQuantity  *float64 `json:"sellable_quantity,omitempty"`
	SuggestedQuantity float64  `json:"suggested_quantity"`
	DataStatus        string   `json:"data_status"`
	ExecutionNotes    []string `json:"execution_notes"`
}

func positionAdvicePlanning(view PositionView) (*PositionExitAssessmentView, *positionAdviceExitPlan) {
	var assessment *PositionExitAssessmentView
	var plan *positionAdviceExitPlan
	if view.ExitAssessment != nil {
		copy := *view.ExitAssessment
		copy.ExitPlan = nil
		if len(copy.Evidence) > 8 {
			copy.Evidence = copy.Evidence[:8]
		}
		assessment = &copy
		if p := view.ExitAssessment.ExitPlan; p != nil {
			plan = &positionAdviceExitPlan{Version: p.Initial.Version, Profile: p.Initial.Profile, SourceHash: view.ExitAssessment.PlanHash,
				InitialStop: p.Initial.StopPrice, CurrentStop: p.CurrentStop, Target: p.Initial.TargetPrice, ExtendedTarget: p.Initial.ExtendedTarget,
				Breakeven: p.BreakevenPrice, Stage: p.Stage, SellableQuantity: p.SellableQuantity, SuggestedQuantity: p.SuggestedQuantity,
				DataStatus: p.DataStatus, ExecutionNotes: p.ExecutionNotes}
		}
	}
	if plan == nil && view.ExitPlanSeed != nil {
		s := view.ExitPlanSeed
		plan = &positionAdviceExitPlan{Version: s.Version, Profile: s.Profile, SourceHash: s.Hash, InitialStop: s.StopPrice,
			Target: s.TargetPrice, ExtendedTarget: s.ExtendedTarget, Stage: "initial_unassessed", DataStatus: s.DataStatus,
			ExecutionNotes: []string{"仅有初始规划，尚无当前退出评估，不推测动态保护或可卖数量"}}
	}
	return assessment, plan
}
