package common

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type testRefusalError struct {
	code string
}

func (e *testRefusalError) Error() string       { return "拒答" }
func (e *testRefusalError) RefusalCode() string { return e.code }

func TestApiErrorPreservesWrappedRefusalCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ApiError(c, errors.Join(errors.New("outer"), &testRefusalError{code: "stale_quote"}))

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["success"] != false || body["code"] != "stale_quote" || body["message"] == "" {
		t.Fatalf("错误包络未保留 code/message: %v", body)
	}
}
