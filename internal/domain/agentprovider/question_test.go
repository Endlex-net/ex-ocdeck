// question_test.go 落实 tasks 2.6：结构化 question 模型纯函数（design D10）——
// EffectiveFields 双视图优先级与合成规则、兼容视图语义。
package agentprovider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestEffectiveFields_FieldsFirst(t *testing.T) {
	p := QuestionPayload{
		Fields: []QuestionField{{Key: "env", Label: "Env", Type: "select"}},
		// 兼容视图同时存在时 Fields 优先，不得混排。
		Questions: []QuestionItem{{Header: "h", Question: "q"}},
	}
	got := p.EffectiveFields()
	if len(got) != 1 || got[0].Key != "env" {
		t.Fatalf("fields-first violated: %+v", got)
	}
}

func TestEffectiveFields_SynthesizeFromQuestions(t *testing.T) {
	p := QuestionPayload{Questions: []QuestionItem{
		{Header: "h1", Question: "q1"},
		{Header: "h2", Question: "q2"},
	}}
	got := p.EffectiveFields()
	// 合成稳定 key question-N（1 起，非上游 ID）；Label=Header、Prompt=Question、
	// Type=text。
	want := []QuestionField{
		{Key: "question-1", Label: "h1", Prompt: "q1", Type: "text"},
		{Key: "question-2", Label: "h2", Prompt: "q2", Type: "text"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("synthesize: got %+v want %+v", got, want)
	}
}

func TestEffectiveFields_BothEmpty(t *testing.T) {
	var p QuestionPayload
	if got := p.EffectiveFields(); got != nil {
		t.Fatalf("empty payload must yield nil, got %+v", got)
	}
}

func TestQuestionField_MetadataRawPreserved(t *testing.T) {
	// Metadata 仅保留未入模型的字段（原文透传，不得成为消费方主路径）。
	raw := json.RawMessage(`{"when":"x"}`)
	f := QuestionField{Key: "k", Metadata: raw}
	if string(f.Metadata) != `{"when":"x"}` {
		t.Fatalf("metadata: %s", f.Metadata)
	}
}
