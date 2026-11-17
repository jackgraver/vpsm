package filter

import (
	"reflect"
	"testing"

	"vpsm-logs/internal/logline"
)

var entries = []logline.Entry{
	{Text: "connection Timeout to db", Level: logline.LevelError},
	{Text: "request ok", Level: logline.LevelInfo},
	{Text: "ERROR in plain text, level unknown"},
	{Text: "slow query timeout", Level: logline.LevelWarn},
	{Text: "verbose", Level: logline.LevelDebug},
}

func TestApply(t *testing.T) {
	cases := []struct {
		name string
		f    Filter
		want []int
	}{
		{"zero value shows all", Filter{}, []int{0, 1, 2, 3, 4}},
		{"lowercase query ignores case", Filter{Query: "timeout"}, []int{0, 3}},
		{"uppercase makes it exact", Filter{Query: "Timeout"}, []int{0}},
		{"min level hides lower and unknown", Filter{MinLevel: logline.LevelWarn}, []int{0, 3}},
		{"both must match", Filter{Query: "timeout", MinLevel: logline.LevelError}, []int{0}},
		{"no match", Filter{Query: "nothing"}, []int{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Apply(entries, nil); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Apply = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMatchesGivesOffsetsIntoTheOriginal(t *testing.T) {
	got := Filter{Query: "ab"}.Matches("xABxab")
	want := []Span{{1, 3}, {4, 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Matches = %v, want %v", got, want)
	}
	if got := (Filter{Query: "AB"}).Matches("xABxab"); !reflect.DeepEqual(got, []Span{{1, 3}}) {
		t.Errorf("case-sensitive Matches = %v", got)
	}
	if got := (Filter{}).Matches("abc"); got != nil {
		t.Errorf("empty query Matches = %v", got)
	}
}

func TestNoiseHiddenUnlessShown(t *testing.T) {
	es := []logline.Entry{{Text: "real"}, {Text: "probe", Noise: true}}
	if got := (Filter{}).Apply(es, nil); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("default = %v, want probes hidden", got)
	}
	if got := (Filter{ShowNoise: true}).Apply(es, nil); !reflect.DeepEqual(got, []int{0, 1}) {
		t.Errorf("ShowNoise = %v", got)
	}
	if got := (Filter{}).ApplyFrom(es, nil, 1, []int{0}); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("ApplyFrom = %v, want only new matches appended", got)
	}
}

func TestQueryMatchesSourceNames(t *testing.T) {
	es := []logline.Entry{
		{Source: 0, Text: "[GIN] 200 GET /money"},
		{Source: 1, Text: "served"},
		{Source: 0, Text: "Backend restarted", Noise: true},
	}
	sources := []string{"simple-track-backend-1", "caddy"}
	if got := (Filter{Query: "backend"}).Apply(es, sources); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("Apply = %v, want the backend's line (and its probe still hidden)", got)
	}
	if got := (Filter{Query: "backend"}).Apply(es, nil); len(got) != 0 {
		t.Errorf("without sources, only text matches: %v", got)
	}
	if got := (Filter{Query: "Backend", ShowNoise: true}).Apply(es, sources); !reflect.DeepEqual(got, []int{2}) {
		t.Errorf("case-sensitive = %v, want only the text match", got)
	}
}
