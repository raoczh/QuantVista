package service

import (
	"testing"

	"quantvista/datasource"
)

func TestRiskGateReviewUsesKnownNameAndBoard(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quote datasource.Quote
		code  string
		want  bool
	}{
		{"估值缺失仍识别ST名称", datasource.Quote{Market: "cn", Symbol: "600001", Name: "*ST本地", Price: 10}, "st", true},
		{"创业板10%不是涨停", datasource.Quote{Market: "cn", Symbol: "300001", Name: "本地股票", Price: 11, ChangePct: 10, High: 11, Low: 11, PrevClose: 10}, "limit_board", false},
		{"创业板20%一字板", datasource.Quote{Market: "cn", Symbol: "300001", Name: "本地股票", Price: 12, ChangePct: 20, High: 12, Low: 12, PrevClose: 10}, "limit_board", true},
		{"振幅缺失不能当零振幅", datasource.Quote{Market: "cn", Symbol: "600001", Name: "本地股票", Price: 11, ChangePct: 10}, "limit_board", false},
		{"主板正常一字板", datasource.Quote{Market: "cn", Symbol: "600001", Name: "本地股票", Price: 11, ChangePct: 10, High: 11, Low: 11, PrevClose: 10}, "limit_board", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flagCodes(computeRiskGate(&tc.quote, nil))
			if (flags[tc.code] != "") != tc.want {
				t.Errorf("风险判断没有遵循已知名称、板块和价格信息: %v", flags)
			}
		})
	}
}
