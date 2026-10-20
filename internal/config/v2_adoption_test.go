// v2_adoption_test.go 落实 tasks 6.2 与 v1-auto-adopt-on-v2 tasks 1.3：主版本
// 提取三类输入（v1/v2/解析失败）与承接开关真值表（默认开、显式关=关、
// 未知值保守=关）。
package config

import "testing"

func TestExtractOpenCodeMajor(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// 裸版本 / v 前缀。
		{"1.18.26", 1},
		{"2.0.28", 2},
		{"v2.0.28", 2},
		{"2", 2},
		// `opencode ` 可执行名前缀（probeOpenCodeVersion 真实输出形态）。
		{"opencode 2.0.28", 2},
		{"opencode v2.0.28", 2},
		{"opencode v1.18.26", 1},
		{"  opencode   v2.0.28  ", 2}, // 多余空白（Fields 分词吸收）
		// 非法输入 → unknown（0）。
		{"", 0},
		{"opencode", 0},
		{"opencode abc", 0},
		{"abc", 0},
		{"v0.1", 0}, // 主版本 0 非正
		{"0.1", 0},  // 主版本 0 非正
		{"-1.2", 0}, // 负号开头非版本 token
		{"opencode v0.9", 0},
	}
	for _, tc := range cases {
		if got := ExtractOpenCodeMajor(tc.in); got != tc.want {
			t.Fatalf("ExtractOpenCodeMajor(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestV2AdoptionSwitch(t *testing.T) {
	if !v2AdoptionEnabled(nil) {
		t.Fatalf("nil lookup must default on")
	}
	if !v2AdoptionEnabled(func(string) (string, bool) { return "", false }) {
		t.Fatalf("unset must default on")
	}
	for _, on := range []string{"1", "true", "ON", " true "} {
		v := on
		if !v2AdoptionEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must enable", on)
		}
	}
	for _, off := range []string{"0", "false", "OFF", " off "} {
		v := off
		if v2AdoptionEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must stay off", off)
		}
	}
	// 未知显式值保守视为关闭（与既有 default 分支一致）。
	for _, unknown := range []string{"", "yes", "2", "yes-please"} {
		v := unknown
		if v2AdoptionEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("unknown value %q must stay off", unknown)
		}
	}
}

func TestDebugSwitch(t *testing.T) {
	if debugEnvEnabled(nil) {
		t.Fatalf("nil lookup must default off")
	}
	if debugEnvEnabled(func(string) (string, bool) { return "", false }) {
		t.Fatalf("unset must default off")
	}
	for _, on := range []string{"1", "true", "ON", " True "} {
		v := on
		if !debugEnvEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must enable", on)
		}
	}
	for _, off := range []string{"", "0", "false", "yes-please"} {
		v := off
		if debugEnvEnabled(func(string) (string, bool) { return v, true }) {
			t.Fatalf("%q must stay off", off)
		}
	}
}
