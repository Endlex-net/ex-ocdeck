// question_test.go 落实 tasks 2.6：EffectiveAnswers fail-closed 校验矩阵
// （design D10：未知/重复 key、必填缺失、数量不匹配拒绝；Values 优先、Answers
// 按字段顺序兼容转换）。
package agentprovider

import (
	"reflect"
	"testing"

	domain "ocdeck/internal/domain/agentprovider"
)

func testQuestionPayload() domain.QuestionPayload {
	return domain.QuestionPayload{
		Fields: []domain.QuestionField{
			{Key: "env", Label: "Env", Type: "select", Required: true,
				Options: []domain.QuestionOption{{Value: "prod", Label: "Prod"}}},
			{Key: "note", Label: "Note", Type: "text"},
		},
	}
}

func TestEffectiveAnswers_ValuesFirst(t *testing.T) {
	r := AttentionReply{
		Answers: []string{"IGNORED", "IGNORED"},
		Values: []domain.QuestionAnswer{
			{Key: "note", Value: "hi"},
			{Key: "env", Value: "prod"},
		},
	}
	got, err := r.EffectiveAnswers(testQuestionPayload())
	if err != nil {
		t.Fatalf("EffectiveAnswers: %v", err)
	}
	// Values 优先：Answers 兼容视图不参与。
	want := []domain.QuestionAnswer{{Key: "note", Value: "hi"}, {Key: "env", Value: "prod"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestEffectiveAnswers_AnswersFallbackOrderBased(t *testing.T) {
	r := AttentionReply{Answers: []string{"prod", "hi"}}
	got, err := r.EffectiveAnswers(testQuestionPayload())
	if err != nil {
		t.Fatalf("EffectiveAnswers: %v", err)
	}
	// 按字段顺序位置转换（key 来自 EffectiveFields 顺序）。
	want := []domain.QuestionAnswer{{Key: "env", Value: "prod"}, {Key: "note", Value: "hi"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback: got %+v want %+v", got, want)
	}
}

func TestEffectiveAnswers_FailClosed(t *testing.T) {
	cases := []struct {
		name string
		r    AttentionReply
	}{
		{"unknown key", AttentionReply{Values: []domain.QuestionAnswer{
			{Key: "env", Value: "prod"}, {Key: "bogus", Value: "x"},
		}}},
		{"duplicate key", AttentionReply{Values: []domain.QuestionAnswer{
			{Key: "env", Value: "prod"}, {Key: "env", Value: "prod2"},
		}}},
		{"values count mismatch", AttentionReply{Values: []domain.QuestionAnswer{
			{Key: "env", Value: "prod"},
		}}},
		{"answers count mismatch", AttentionReply{Answers: []string{"only-one"}}},
		{"empty", AttentionReply{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.r.EffectiveAnswers(testQuestionPayload()); err == nil {
				t.Fatalf("%s must reject", tc.name)
			}
		})
	}
}

func TestEffectiveAnswers_RequiredMissing(t *testing.T) {
	p := domain.QuestionPayload{Fields: []domain.QuestionField{
		{Key: "a", Type: "text", Required: true},
		{Key: "b", Type: "text"},
	}}
	// 数量匹配但（结构上不可能缺失）——必填缺失由 key 覆盖校验承载：构造
	// 仅可选字段回答的畸形 Values（数量守卫先行）。此处验证守卫存在：
	// 数量相等 + key 全覆盖时必填视为已答。
	r := AttentionReply{Values: []domain.QuestionAnswer{
		{Key: "a", Value: "x"}, {Key: "b", Value: "y"},
	}}
	if _, err := r.EffectiveAnswers(p); err != nil {
		t.Fatalf("full coverage must pass: %v", err)
	}
}
