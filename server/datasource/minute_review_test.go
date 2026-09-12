package datasource

import (
	"encoding/json"
	"testing"
)

func TestMinuteParserRejectsInvalidAndDuplicateFacts(t *testing.T) {
	for _, invalid := range [][]string{
		{"202613090940", "10", "10", "10", "10", "1"},
		{"202609090940", "10", "10", "9", "10", "1"},
		{"202609090940", "NaN", "10", "10", "10", "1"},
		{"202609090940", "10", "10", "10", "10", "-1"},
	} {
		valid := []string{"202609090935", "10", "10", "10", "10", "1"}
		raw, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"sh600901": map[string]any{"m5": [][]string{valid, valid, invalid}}}})
		if err != nil {
			t.Fatal(err)
		}
		bars, err := parseMin5Response(raw, "sh600901")
		if err != nil || len(bars) != 1 {
			t.Errorf("非法或重复分钟不能进入完整性分母：invalid=%v bars=%+v err=%v", invalid, bars, err)
		}
	}
}
