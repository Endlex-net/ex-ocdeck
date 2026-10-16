// v2_adoption_test.go 落实 tasks 6.2/6.7：主版本提取三类输入（v1/v2/解析失败）
// 与承接开关两态（默认关）。
package config

import "testing"

func TestExtractOpenCodeMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"1.18.26", 1},
		{"2.0.28", 2},
		{"v2.0.28", 2},
		{"opencode 2.0.28", 0}, // 前导非版本数字 → unknown
		{"", 0},
		{"abc", 0},
		{"0.1", 0},
		{"2", 2},
	}
	for _, tc := range cases {
		if got := ExtractOpenCodeMajor(tc.in); got != tc.want {
			t.Fatalf("ExtractOpenCodeMajor(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestV2AdoptionSwitch(t *testing.T) {
	if v2AdoptionEnabled(nil) {
		t.Fatalf("nil lookup must default off")
	}
	if v2AdoptionEnabled(func(string) (string, bool) { return "", false }) {
		t.Fatalf("unset must default off")
	}
	for _, on := range []string{"1", "true", "ON", " true "} {
		v := on
		if !v2AdoptionEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must enable", on)
		}
	}
	for _, off := range []string{"", "0", "false", "yes-please"} {
		v := off
		if v2AdoptionEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must stay off", off)
		}
	}
}
