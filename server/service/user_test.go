package service

import "testing"

func TestRecommendationTypeForHorizon(t *testing.T) {
	cases := []struct {
		name    string
		horizon string
		want    string
	}{
		{name: "short term", horizon: HorizonShortTerm, want: RecommendationTypeShortTerm},
		{name: "mid term", horizon: HorizonMidTerm, want: RecommendationTypeLongTerm},
		{name: "long term", horizon: HorizonLongTerm, want: RecommendationTypeLongTerm},
		{name: "unknown defaults to long term", horizon: "", want: RecommendationTypeLongTerm},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecommendationTypeForHorizon(tc.horizon); got != tc.want {
				t.Fatalf("RecommendationTypeForHorizon(%q) = %q, want %q", tc.horizon, got, tc.want)
			}
		})
	}
}
