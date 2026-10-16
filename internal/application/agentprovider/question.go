// question.go 实现方案 B（design D10）的回复侧纯函数：AttentionReply →
// key-based 答案视图。与 domain EffectiveFields 同族（不涉及 wire、无 IO）；
// 落位说明：EffectiveAnswers 是 AttentionReply（application 类型）的方法，
// domain 包不能反向承载——纯函数语义不变，位置随类型走。
package agentprovider

import (
	"fmt"

	domain "ocdeck/internal/domain/agentprovider"
)

// EffectiveAnswers 返回 key-based 回复视图（design D10 纯函数，fail-closed）：
//   - Values 非空（主视图）：逐条校验 Key 必须存在于 payload 的主视图字段
//     （EffectiveFields）、Key 不得重复；必填字段缺失拒绝；数量与字段数不一致
//     拒绝（无部分回复语义）。
//   - Values 为空（兼容回退）：Answers 按字段顺序位置转换；数量不匹配拒绝；
//     必填字段缺失拒绝。
//
// 任何校验失败返回参数错误，调用方不得产生任何 wire 副作用。
func (r AttentionReply) EffectiveAnswers(p domain.QuestionPayload) ([]domain.QuestionAnswer, error) {
	fields := p.EffectiveFields()
	if len(r.Values) > 0 {
		if len(r.Values) != len(fields) {
			return nil, fmt.Errorf("question reply: values 数 %d 与字段数 %d 不一致", len(r.Values), len(fields))
		}
		known := make(map[string]bool, len(fields))
		for _, f := range fields {
			known[f.Key] = true
		}
		answered := make(map[string]bool, len(r.Values))
		out := make([]domain.QuestionAnswer, 0, len(r.Values))
		for _, a := range r.Values {
			if !known[a.Key] {
				return nil, fmt.Errorf("question reply: 未知字段 key %q", a.Key)
			}
			if answered[a.Key] {
				return nil, fmt.Errorf("question reply: 重复字段 key %q", a.Key)
			}
			answered[a.Key] = true
			out = append(out, a)
		}
		if err := requireFieldsAnswered(fields, answered); err != nil {
			return nil, err
		}
		return out, nil
	}
	// 兼容回退：Answers 按字段顺序转换。
	if len(r.Answers) != len(fields) {
		return nil, fmt.Errorf("question reply: answers 数 %d 与字段数 %d 不一致", len(r.Answers), len(fields))
	}
	answered := make(map[string]bool, len(fields))
	out := make([]domain.QuestionAnswer, 0, len(fields))
	for i, f := range fields {
		answered[f.Key] = true
		out = append(out, domain.QuestionAnswer{Key: f.Key, Value: r.Answers[i]})
	}
	if err := requireFieldsAnswered(fields, answered); err != nil {
		return nil, err
	}
	return out, nil
}

// requireFieldsAnswered 校验必填字段均有对应回复（answered 为已回答 key 集）。
func requireFieldsAnswered(fields []domain.QuestionField, answered map[string]bool) error {
	for _, f := range fields {
		if f.Required && !answered[f.Key] {
			return fmt.Errorf("question reply: 必填字段 %q 缺失回复", f.Key)
		}
	}
	return nil
}
