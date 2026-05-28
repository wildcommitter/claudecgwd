package bridge

import (
	"reflect"
	"testing"
)

func TestParseChoiceReply(t *testing.T) {
	cases := []struct {
		name     string
		reply    string
		nOptions int
		multi    bool
		wantIdx  []int
		wantFree string
	}{
		{"single number", "2", 3, false, []int{1}, ""},
		{"single ignores extra", "2 3", 3, false, []int{1}, ""},
		{"multi comma", "1,3", 3, true, []int{0, 2}, ""},
		{"multi spaces", "1 2 3", 3, true, []int{0, 1, 2}, ""},
		{"out of range -> free text", "5", 3, false, nil, "5"},
		{"non-numeric -> free text", "the second one", 3, false, nil, "the second one"},
		{"multi drops out-of-range", "2,9", 3, true, []int{1}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseChoiceReply(c.reply, c.nOptions, c.multi)
			if c.wantFree != "" {
				if got.FreeText != c.wantFree || len(got.Indices) != 0 {
					t.Fatalf("got %+v, want FreeText=%q", got, c.wantFree)
				}
				return
			}
			if !reflect.DeepEqual(got.Indices, c.wantIdx) || got.FreeText != "" {
				t.Fatalf("got %+v, want Indices=%v", got, c.wantIdx)
			}
		})
	}
}
