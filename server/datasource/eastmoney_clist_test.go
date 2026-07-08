package datasource

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// clist 快照一页的真实响应样本（2026-07-08 实测裁剪）：
// 000002 名称含全角空格；000003 为停牌行（价格字段 "-"、f18 昨收有值、f124 旧时间戳）。
const clistSpotFixtureArray = `{"rc":0,"rt":6,"svr":1,"lt":1,"full":1,"data":{"total":5535,"diff":[
{"f2":10.47,"f3":-0.29,"f5":805221,"f6":840465778.05,"f8":0.41,"f12":"000001","f14":"平安银行","f15":10.51,"f16":10.35,"f17":10.46,"f18":10.5,"f124":1783409697},
{"f2":2.99,"f3":-2.61,"f5":1107307,"f6":332291104.94,"f8":1.14,"f12":"000002","f14":"万  科Ａ","f15":3.06,"f16":2.97,"f17":3.06,"f18":3.07,"f124":1783409697},
{"f2":"-","f3":"-","f5":"-","f6":"-","f8":0.0,"f12":"000003","f14":"PT金田A","f15":"-","f16":"-","f17":"-","f18":2.71,"f124":1783382400}
]}}`

const clistSpotFixtureObject = `{"data":{"total":2,"diff":{
"1":{"f2":2.99,"f3":-2.61,"f5":1107307,"f6":332291104.94,"f8":1.14,"f12":"000002","f14":"万科A","f15":3.06,"f16":2.97,"f17":3.06,"f18":3.07,"f124":1783409697},
"0":{"f2":10.47,"f3":-0.29,"f5":805221,"f6":840465778.05,"f8":0.41,"f12":"000001","f14":"平安银行","f15":10.51,"f16":10.35,"f17":10.46,"f18":10.5,"f124":1783409697}
}}}`

func TestParseClistSpotArray(t *testing.T) {
	rows, total, err := parseClistSpot([]byte(clistSpotFixtureArray))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if total != 5535 {
		t.Fatalf("total = %d, want 5535", total)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3（停牌行也保留）", len(rows))
	}
	r0 := rows[0]
	if r0.Symbol != "000001" || r0.Name != "平安银行" || r0.Price != 10.47 ||
		r0.PrevClose != 10.5 || r0.Volume != 805221 || r0.TurnoverRate != 0.41 ||
		r0.DataTime != 1783409697 {
		t.Fatalf("row0 字段不符: %+v", r0)
	}
	if r0.Open != 10.46 || r0.High != 10.51 || r0.Low != 10.35 || r0.ChangePct != -0.29 {
		t.Fatalf("row0 OHLC 不符: %+v", r0)
	}
	// 停牌行：价格字段 0，昨收保留（除权初筛需要）。
	r2 := rows[2]
	if r2.Symbol != "000003" || r2.Price != 0 || r2.Volume != 0 {
		t.Fatalf("停牌行应保留且价格为 0: %+v", r2)
	}
	if r2.PrevClose != 2.71 {
		t.Fatalf("停牌行昨收应保留: %+v", r2)
	}
}

func TestParseClistSpotObject(t *testing.T) {
	rows, total, err := parseClistSpot([]byte(clistSpotFixtureObject))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("total=%d rows=%d, want 2/2", total, len(rows))
	}
	// 对象态按数字键排序还原顺序。
	if rows[0].Symbol != "000001" || rows[1].Symbol != "000002" {
		t.Fatalf("对象态顺序错误: %s, %s", rows[0].Symbol, rows[1].Symbol)
	}
}

// spotPageJSON 构造一页 pn 从 startIdx 开始的 n 行假数据。
func spotPageJSON(total, startIdx, n int) string {
	var b strings.Builder
	b.WriteString(`{"data":{"total":` + strconv.Itoa(total) + `,"diff":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		sym := "6" + strings.Repeat("0", 5-len(strconv.Itoa(startIdx+i))) + strconv.Itoa(startIdx+i)
		b.WriteString(`{"f2":10.0,"f3":1.0,"f5":100,"f6":1000.0,"f8":1.0,"f12":"` + sym +
			`","f14":"股票` + strconv.Itoa(startIdx+i) + `","f15":10.5,"f16":9.5,"f17":9.8,"f18":9.9,"f124":1783409697}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

func pnOf(rawURL string) int {
	u, _ := url.Parse(rawURL)
	n, _ := strconv.Atoi(u.Query().Get("pn"))
	return n
}

func TestGetCNSpotSnapshotPaging(t *testing.T) {
	// total=250：页1 100 行、页2 100 行、页3 50 行，凑满即停（不再翻第 4 页）。
	calls := 0
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		calls++
		switch pn := pnOf(rawURL); pn {
		case 1:
			return []byte(spotPageJSON(250, 0, 100)), 200, nil
		case 2:
			return []byte(spotPageJSON(250, 100, 100)), 200, nil
		case 3:
			return []byte(spotPageJSON(250, 200, 50)), 200, nil
		default:
			t.Fatalf("不应翻到第 %d 页", pn)
			return nil, 0, nil
		}
	}
	rows, err := e.GetCNSpotSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(rows) != 250 {
		t.Fatalf("rows = %d, want 250", len(rows))
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestGetCNSpotSnapshotPageRetry(t *testing.T) {
	// 第 2 页首次网络失败，页级重试成功，整轮仍成功。
	failedOnce := false
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		switch pn := pnOf(rawURL); pn {
		case 1:
			return []byte(spotPageJSON(150, 0, 100)), 200, nil
		case 2:
			if !failedOnce {
				failedOnce = true
				return nil, 0, errors.New("EOF")
			}
			return []byte(spotPageJSON(150, 100, 50)), 200, nil
		default:
			return []byte(`{"data":{"total":150,"diff":[]}}`), 200, nil
		}
	}
	rows, err := e.GetCNSpotSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(rows) != 150 {
		t.Fatalf("rows = %d, want 150", len(rows))
	}
}

func TestGetCNSpotSnapshotIncomplete(t *testing.T) {
	// 页 2 起返回空页：100/250 < 90%，半截快照必须整轮拒绝（防静默缺口）。
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		if pnOf(rawURL) == 1 {
			return []byte(spotPageJSON(250, 0, 100)), 200, nil
		}
		return []byte(`{"data":{"total":250,"diff":[]}}`), 200, nil
	}
	if _, err := e.GetCNSpotSnapshot(context.Background()); err == nil {
		t.Fatal("半截快照应报错")
	}
}
