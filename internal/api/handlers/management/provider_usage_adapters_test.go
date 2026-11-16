package management

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestOpenRouterKeyWindow(t *testing.T) {
	num := func(v float64) *float64 { return &v }
	cases := []struct {
		name                            string
		data                            string
		used, limit, remaining, percent *float64
	}{
		{"null limit stays unknown", `{"usage":3.5,"limit":null,"limit_remaining":null,"limit_reset":null}`, num(3.5), nil, nil, nil},
		{"absent limit stays unknown", `{"usage":3.5}`, num(3.5), nil, nil, nil},
		{"resetting limit uses remaining", `{"usage":57.5,"limit":10,"limit_remaining":2.5,"limit_reset":"monthly"}`, num(7.5), num(10), num(2.5), num(75)},
		{"zero limit has no percentage", `{"usage":57.5,"limit":0,"limit_remaining":0,"limit_reset":"monthly"}`, num(0), num(0), num(0), nil},
		{"missing remaining falls back to all-time usage", `{"usage":4,"limit":16}`, num(4), num(16), nil, num(25)},
		{"overspent limit", `{"usage":30,"limit":10,"limit_remaining":-2}`, num(12), num(10), num(-2), num(120)},
	}
	same := func(got, want *float64) bool {
		if got == nil || want == nil {
			return got == want
		}
		return *got == *want
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			window, ok := openRouterKeyWindow(gjson.Parse(tc.data))
			if !ok || window.ID != "key" || window.Unit != "usd" {
				t.Fatalf("window = %+v ok=%v", window, ok)
			}
			if !same(window.Used, tc.used) || !same(window.Limit, tc.limit) || !same(window.Remaining, tc.remaining) || !same(window.UsedPercent, tc.percent) {
				t.Fatalf("window = %+v", window)
			}
		})
	}
	if _, ok := openRouterKeyWindow(gjson.Parse(`{"limit":10}`)); ok {
		t.Fatal("a key without usage must not produce a window")
	}
}
