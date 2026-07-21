package controller

import (
	"encoding/json"
	"fmt"
	"testing"

	"quantvista/service"
)

func TestQaStreamErrorLinePreservesRefusalCode(t *testing.T) {
	err := fmt.Errorf("outer: %w", &service.RefusalError{Code: service.RefusalStaleQuote, Msg: "行情过期"})
	line := qaStreamErrorLine(err)
	if line.Status != "error" || line.RefusalCode != service.RefusalStaleQuote || line.Message == "" {
		t.Fatalf("QA NDJSON 错误行未保留拒答码/文案: %+v", line)
	}
	payload, marshalErr := json.Marshal(line)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	var decoded map[string]any
	if unmarshalErr := json.Unmarshal(payload, &decoded); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if decoded["refusal_code"] != service.RefusalStaleQuote {
		t.Fatalf("序列化后的 QA NDJSON 未带 refusal_code: %s", payload)
	}
}
