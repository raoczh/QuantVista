package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetailTemplatesExposeDeterministicConditions(t *testing.T) {
	view, err := NewScreenerService().Strategies(0)
	if err != nil {
		t.Fatalf("Strategies: %v", err)
	}
	if len(view.RetailTemplates) != 4 {
		t.Fatalf("retail templates = %d, want 4", len(view.RetailTemplates))
	}
	for _, template := range view.RetailTemplates {
		if template.Version != retailTemplateVersion || template.Scenario == "" || template.Risk == "" || template.DataRequirements == "" {
			t.Fatalf("template metadata incomplete: %+v", template)
		}
		if len(template.Params) == 0 || len(template.Params) > 3 {
			t.Fatalf("template %s params = %d", template.Key, len(template.Params))
		}
		if len(template.Conditions) == 0 {
			t.Fatalf("template %s has no conditions", template.Key)
		}
		resolvedA, valuesA, err := resolveRetailTemplate(template.Key, template.Version, nil)
		if err != nil {
			t.Fatalf("resolve %s: %v", template.Key, err)
		}
		resolvedB, valuesB, err := resolveRetailTemplate(template.Key, template.Version, valuesA)
		if err != nil {
			t.Fatalf("resolve %s again: %v", template.Key, err)
		}
		if resolvedA.Hash != resolvedB.Hash || resolvedA.Hash == "" {
			t.Fatalf("template %s hash is not deterministic", template.Key)
		}
		left, _ := json.Marshal(valuesA)
		right, _ := json.Marshal(valuesB)
		if string(left) != string(right) {
			t.Fatalf("template %s defaults changed", template.Key)
		}
	}
}

func TestRetailTemplateParameterBoundsAndFrozenJob(t *testing.T) {
	if _, _, err := resolveRetailTemplate("low-price-steady", 1, map[string]float64{"max_price": 101}); err == nil {
		t.Fatal("expected upper-bound error")
	}
	if _, _, err := resolveRetailTemplate("low-price-steady", 2, nil); err == nil {
		t.Fatal("expected version error")
	}
	if _, _, err := resolveRetailTemplate("low-price-steady", 1, map[string]float64{"unknown": 1}); err == nil {
		t.Fatal("expected unknown-param error")
	}
	if _, err := NewScreenerService().resolveStrategy(1, ScanRequest{
		TemplateKey: "low-price-steady", StrategyKey: "low-volatility",
	}); err == nil || !strings.Contains(err.Error(), "仅指定一种") {
		t.Fatalf("expected exclusive source error, got %v", err)
	}

	seed, _, err := NewScreenerService().prepareScanJob(1, ScanRequest{
		TemplateKey: "low-price-steady", TemplateVersion: 1,
		TemplateParams: map[string]float64{"max_price": 20}, Limit: 20,
	})
	if err != nil {
		t.Fatalf("prepareScanJob: %v", err)
	}
	if seed.StrategyKey != "retail:low-price-steady:v1" || seed.StrategyHash == "" {
		t.Fatalf("unexpected seed: %+v", seed)
	}
	var normalized ScanRequest
	if err := json.Unmarshal([]byte(seed.RequestJSON), &normalized); err != nil {
		t.Fatalf("normalized request: %v", err)
	}
	if normalized.Tree == nil || normalized.TemplateKey != "low-price-steady" || normalized.TemplateParams["max_price"] != 20 {
		t.Fatalf("template facts not frozen: %+v", normalized)
	}
	execution, err := frozenScanRequest(seed.RequestJSON)
	if err != nil {
		t.Fatalf("frozenScanRequest: %v", err)
	}
	if execution.Tree == nil || execution.TemplateKey != "" || execution.TemplateParams != nil {
		t.Fatalf("worker should execute only frozen tree: %+v", execution)
	}
}
