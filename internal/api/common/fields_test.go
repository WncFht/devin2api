package common

import (
	"slices"
	"testing"
)

func TestUnconsumedFields(t *testing.T) {
	consumed := map[string]bool{"model": true, "messages": true}
	tests := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "reports only unconsumed top-level keys",
			data: `{"model":"x","messages":[{"role":"user"}],"reasoning":{"effort":"high"},"store":false}`,
			want: []string{"field:reasoning", "field:store"},
		},
		{
			name: "skips nested objects and arrays in values",
			data: `{"a":{"b":{"c":[1,2,{"d":"x"}]}},"z":[true,false,null]}`,
			want: []string{"field:a", "field:z"},
		},
		{
			name: "braces and quotes inside string values stay opaque",
			data: `{"a":"}{\"\\\"}","b":2}`,
			want: []string{"field:a", "field:b"},
		},
		{
			name: "escaped key names decode before matching",
			data: `{"weird":1,"model":"m"}`,
			want: []string{"field:weird"},
		},
		{
			name: "duplicate unconsumed keys collapse like the map form",
			data: `{"x":1,"x":2,"model":"m"}`,
			want: []string{"field:x"},
		},
		{name: "all consumed yields nothing", data: `{"model":"m","messages":[]}`, want: nil},
		{name: "empty object yields nothing", data: `{}`, want: nil},
		{name: "whitespace padded", data: " \t\n { \"a\" : 1 } ", want: []string{"field:a"}},
		{name: "scalar literal values", data: `{"a":12.5e3,"b":null,"c":true}`, want: []string{"field:a", "field:b", "field:c"}},
		{name: "non-object array returns nil", data: `[{"a":1}]`, want: nil},
		{name: "non-object literal returns nil", data: `"just a string"`, want: nil},
		{name: "truncated object returns nil", data: `{"a":`, want: nil},
		{name: "unquoted key returns nil", data: `{a:1}`, want: nil},
		{name: "empty input returns nil", data: ``, want: nil},
	}
	for _, test := range tests {
		got := UnconsumedFields([]byte(test.data), consumed)
		if !slices.Equal(got, test.want) {
			t.Errorf("%s: UnconsumedFields = %v, want %v", test.name, got, test.want)
		}
	}
}
