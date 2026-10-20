// credentials.go 实现 D3 派生凭据原语（opencode-v1-spi-provider tasks 4.1）。
// 落点说明：原语最初随 C1 落在 opencode v1 适配器包内；C2c 起生产 spawn
// （task 层 startRuntimeWithPortRetry）与迁移导入 runner 均需消费，而 grep 口径
// 禁止 task/application import opencode——原语为纯函数（HMAC + env/文件解析），
// 无基础设施依赖，落 application/agentprovider 为唯一单点。
//
// 派生公式（envelope version=1 固定）：
//
//	password = base64url_nopad(HMAC-SHA256(
//	    key = master_secret,                        // UTF-8 原始字节
//	    msg = "ocdeck-agent-v1" || 0x00 || provider_id || 0x00 || task_id
//	))[:32]   // 取前 32 个 base64url 字符
package agentprovider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// credKDFLabel 是派生公式的固定标签（design D3 逐字）。
const credKDFLabel = "ocdeck-agent-v1"

// DerivedCredKind 是派生凭据的非秘密来源标记（spawn 注入 OCDECK_CRED_KIND=
// derived，供迁移导入 runner 的来源判定；D3/D6）。
const DerivedCredKind = "derived"

// LegacyCredKind 是迁移导入的存量随机凭据标记（binding data.auth.kind="legacy"；
// 该任务下次激活/重建起切换为派生凭据——D3 legacy 例外）。
const LegacyCredKind = "legacy"

// CredKindEnvKey 是来源标记注入 spawn env 的键名。
const CredKindEnvKey = "OCDECK_CRED_KIND"

// MasterSecretEnv 是 master secret 的环境变量名（解析顺序第一优先）。
const MasterSecretEnv = "OCDECK_MASTER_SECRET"

// masterSecretFile 是 master secret 的配置文件路径（相对 HOME）。
const masterSecretFile = ".config/ocdeck/master_secret"

// DeriveRuntimePassword 按 D3 固定公式派生运行时密码（同任务恒定；32 字符
// base64url 无填充）。确定性纯函数，不读任何外部状态。
func DeriveRuntimePassword(masterSecret, providerID, taskID string) string {
	mac := hmac.New(sha256.New, []byte(masterSecret))
	mac.Write([]byte(credKDFLabel))
	mac.Write([]byte{0x00})
	mac.Write([]byte(providerID))
	mac.Write([]byte{0x00})
	mac.Write([]byte(taskID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
}

// ResolveMasterSecret 按固定顺序解析 master secret（D3）：环境变量
// OCDECK_MASTER_SECRET 优先（显式设置但 TrimSpace 后为空 → fail-closed，
// 不回退文件），其次 ~/.config/ocdeck/master_secret（TrimSpace 读取；0600
// 权限由部署方保证，D3）。缺失/读取失败/空值 → fail-closed 返回错误
// （涉及凭据的操作不得继续）。
func ResolveMasterSecret() (string, error) {
	if v, ok := os.LookupEnv(MasterSecretEnv); ok {
		v = strings.TrimSpace(v)
		if v == "" {
			return "", fmt.Errorf("agentprovider: %s 为空", MasterSecretEnv)
		}
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("agentprovider: resolve master secret: home dir: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, masterSecretFile))
	if err != nil {
		return "", fmt.Errorf("agentprovider: resolve master secret: read %s: %w", masterSecretFile, err)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", errors.New("agentprovider: resolve master secret: 文件内容为空")
	}
	return v, nil
}

// EnsureMasterSecret 解析或首次自动生成 master secret（D3 修订：文件缺失时
// 首启自动供给——存量安装升级不再崩启动，用户批准）：
//   - ResolveMasterSecret() 成功 → 直接返回（env 优先语义不变，测试注入路径
//     不受影响）；
//   - env 显式设置时的失败（显式空串）、文件存在但空白、读盘其他错误 →
//     维持原 fail-closed 错误（显式配置错误不自动修复）；
//   - 仅「文件不存在」（且 env 未设置）→ 生成 crypto/rand 32 字节 hex（64
//     字符）：MkdirAll 父目录 0700 + OpenFile O_WRONLY|O_CREATE|O_EXCL 0600；
//     O_EXCL 冲突（并发进程已生成）→ 重读文件返回其内容；写盘其他错误
//     fail-closed。生成成功向 stderr 打印一行提示（含路径，不含 secret 本体）。
func EnsureMasterSecret() (string, error) {
	v, err := ResolveMasterSecret()
	if err == nil {
		return v, nil
	}
	// env 显式设置时的失败属显式配置错误（如显式空串）——不自动生成。
	if _, ok := os.LookupEnv(MasterSecretEnv); ok {
		return "", err
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", herr
	}
	path := filepath.Join(home, masterSecretFile)
	if merr := os.MkdirAll(filepath.Dir(path), 0o700); merr != nil {
		return "", fmt.Errorf("agentprovider: ensure master secret: mkdir: %w", merr)
	}
	raw := make([]byte, 32)
	if _, rerr := rand.Read(raw); rerr != nil {
		return "", fmt.Errorf("agentprovider: ensure master secret: rand: %w", rerr)
	}
	hexed := hex.EncodeToString(raw)
	f, cerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if cerr != nil {
		if errors.Is(cerr, fs.ErrExist) {
			// 并发进程已生成 → 读取现有文件返回（ResolveMasterSecret 语义）。
			return ResolveMasterSecret()
		}
		return "", fmt.Errorf("agentprovider: ensure master secret: write %s: %w", masterSecretFile, cerr)
	}
	if _, werr := f.WriteString(hexed); werr != nil {
		_ = f.Close()
		return "", fmt.Errorf("agentprovider: ensure master secret: write %s: %w", masterSecretFile, werr)
	}
	if clerr := f.Close(); clerr != nil {
		return "", fmt.Errorf("agentprovider: ensure master secret: write %s: %w", masterSecretFile, clerr)
	}
	fmt.Fprintf(os.Stderr, "agentprovider: master secret 首次生成：%s（随机 32 字节 hex，0600）\n", path)
	return hexed, nil
}
