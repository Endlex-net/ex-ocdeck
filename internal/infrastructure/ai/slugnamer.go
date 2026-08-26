// SlugNamer：将任务名提炼为英文 kebab-case slug 的 LLM adapter（design.md D3/D4）。
//
// 职责组合：配置 Store 可用性判定 + Completer + 三道清洗门禁 + 注入的 fallback。
// Slug 永不返回 error：任一失败/调用出错/超时 → 调用 fallback。
// AI 未配置（Store 判定 configured=false）时零网络调用直接 fallback。
// 不 import internal/task（避免循环依赖）；fallback 由 wiring 注入。

package ai

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"
)

// maxTaskNameChars 任务名截断上限（design.md D4：500 字符）。
const maxTaskNameChars = 500

// slugTimeoutSlug Slug 调用 LLM 的超时上限（design.md：≤10s）。
const slugTimeout = 10 * time.Second

// slugPrompt 设计.md D4：「将任务名提炼为 2-5 个词的英文 kebab-case 短语，只输出该短语」。
const slugPrompt = "Convert the following task name into a 2-5 word English kebab-case phrase. " +
	"Output only that phrase, nothing else."

// slugNamerMaxTokens 留给 LLM 的输出 token 上限。
// 最终短语很短，但 reasoning/thinking 模型会先消耗大量 token 在思考块上
// （实测 deepseek 经 Anthropic 协议：32 预算被 thinking 吃满、响应截断、无 text 块），
// 故给足余量；输出长度由 D3 清洗门禁约束。
const slugNamerMaxTokens = 1024

// slugFormatRe D3 格式门禁：≤50 字符、可作 ocdeck/ 后缀、过 git check-ref-format。
var slugFormatRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`)

// slugDenySet D3 语义兜底词表（固化集合）。
var slugDenySet = map[string]struct{}{
	"task":     {},
	"new":      {},
	"untitled": {},
}

// SlugNamer 实现 task.BranchNamer（接口定义在 Lane C，本包不依赖 task 包）。
//
// 通过 fallback func 注入回退逻辑（wiring 时传 task.Slugify），避免 import internal/task
// 造成循环依赖。
type SlugNamer struct {
	store    *Store
	fallback func(string) string

	// completerFactory 仅供测试注入；nil 时使用 NewCompleter（生产路径）。
	completerFactory func(ProviderConfig) (Completer, error)
}

// NewSlugNamer 构造 SlugNamer。fallback MUST 非 nil（wiring 保证）。
func NewSlugNamer(store *Store, fallback func(string) string) *SlugNamer {
	return &SlugNamer{store: store, fallback: fallback}
}

// Slug 返回分支 slug，永不返回 error（design.md D4）。
//
// 单次调用全程使用同一快照（无 data race、无半新半旧混用）。
// 未配置 → 零网络调用 → fallback。
// 调用 LLM 失败/超时/清洗未通过 → fallback。
func (n *SlugNamer) Slug(ctx context.Context, taskName string) string {
	if n == nil || n.fallback == nil {
		// 防御：wiring 应保证非 nil；万一缺失仍不 panic，给一个稳定结果。
		return fallbackSlugify(taskName)
	}
	sn := n.store.Snapshot()
	if sn == nil || !sn.configured {
		return n.fallback(taskName)
	}

	slug, ok := n.callLLM(ctx, sn.cfg, taskName)
	if !ok {
		return n.fallback(taskName)
	}
	if cleaned, ok := cleanSlug(slug); ok {
		return cleaned
	}
	return n.fallback(taskName)
}

// callLLM 发起单次 LLM 调用并返回原始文本。失败返回 ok=false（不返回 error）。
func (n *SlugNamer) callLLM(ctx context.Context, cfg ProviderConfig, taskName string) (string, bool) {
	factory := NewCompleter
	if n.completerFactory != nil {
		factory = n.completerFactory
	}
	completer, err := factory(cfg)
	if err != nil {
		// 配置层应已拒绝未知 provider；此处防御性返回失败。
		return "", false
	}

	cctx, cancel := context.WithTimeout(ctx, slugTimeout)
	defer cancel()

	name := taskName
	// 按字符数截断 500（任务名可能含中文，按 rune 截断更安全）。
	runes := []rune(name)
	if len(runes) > maxTaskNameChars {
		name = string(runes[:maxTaskNameChars])
	}

	resp, err := completer.Complete(cctx, Request{
		System:    slugPrompt,
		User:      name,
		MaxTokens: slugNamerMaxTokens,
	})
	if err != nil {
		// 不向用户返回、不阻断；仅日志便于诊断（不含 api_key）。
		log.Printf("ai slug completer failed: %v", err)
		return "", false
	}
	return resp.Text, true
}

// cleanSlug D3 三道清洗门禁。通过返回 (slug, true)；任一失败返回 ("", false)。
func cleanSlug(raw string) (string, bool) {
	// 1. 取首行、去首尾空白、去包裹引号（" ' `）、去尾部标点（.,;:）、lowercase。
	s := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	s = strings.Trim(s, "\"'`")
	s = strings.TrimRight(s, ".,;:")
	s = strings.ToLower(s)

	// 2. 格式：必须匹配 slugFormatRe。
	if !slugFormatRe.MatchString(s) {
		return "", false
	}

	// 3. 语义兜底：空或命中固化词表。
	if s == "" {
		return "", false
	}
	if _, bad := slugDenySet[s]; bad {
		return "", false
	}
	return s, true
}

// fallbackSlugify 仅在 fallback 未注入时使用（防御性），行为与 task.Slugify 等价。
// 正常 wiring 下不会走到此处。
func fallbackSlugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := true
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "task"
	}
	return out
}