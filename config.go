// config.go: .env 解析 + 配置默认值 + 上游 key 的运行时热更新
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Config 持有运行时配置。realKey 只在启动迁移期用（种进 anthropic-official 服务商），
// 运行时上游 key 由「服务商」页管理，不再经 .env 热更新；但仍用 mutex 守护以防残留调用。
// admin token 不允许热更新（避免管理员把自己锁死），保持普通字段即可。
type Config struct {
	mu         sync.RWMutex
	realKey    string
	AdminToken string
	HideCC     bool
	envPath    string // loadConfig 实际命中的 .env 路径（用于热更新时写回）
}

// GetRealKey 返回当前上游 key。可能为空字符串——调用方负责处理空 key 的请求拒绝。
func (c *Config) GetRealKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.realKey
}

// SetRealKey 原子更新内存中的 key 并同步写回 .env。
// newKey 为空字符串等同于清除（.env 里该行被删除）。
// 写盘时强制 0600 权限，避免别的用户读到。
func (c *Config) SetRealKey(newKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := rewriteEnvKey(c.envPath, "key", newKey); err != nil {
		return err
	}
	c.realKey = newKey
	return nil
}

func loadConfig() *Config {
	cfg := &Config{AdminToken: "admin-secret-change-me"}

	// 先找当前目录的 .env（标准位置）；找不到再退到上一级（兼容旧布局）
	path := ".env"
	data, err := os.ReadFile(path)
	if err != nil {
		path = "../.env"
		data, err = os.ReadFile(path)
		if err != nil {
			log.Fatalf("read .env: %v (looked in . and ..)", err)
		}
	}
	cfg.envPath = path

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "key=") {
			cfg.realKey = strings.TrimPrefix(line, "key=")
		}
		if strings.HasPrefix(line, "admin_token=") {
			cfg.AdminToken = strings.TrimPrefix(line, "admin_token=")
		}
	}

	// .env 同时存上游 key 和 admin token，世界可读会泄密。启动时强制收紧到 0600。
	if err := os.Chmod(path, 0600); err != nil {
		log.Printf("chmod %s 0600 failed: %v (continuing, but file may be world-readable)", path, err)
	}

	// realKey 允许启动时为空：管理员可先空跑，再到「服务商」页配置上游。
	if cfg.realKey == "" {
		log.Printf("WARNING: upstream key not set in .env — forward requests will be rejected until an upstream is configured in /admin/providers")
	}
	return cfg
}

// rewriteEnvKey 在 .env 里 upsert 一行 `name=value`：
//   - 已存在则替换那一行（保留前后其他行原样）
//   - 不存在且 value 非空则追加到文件末尾
//   - value 为空时该行被移除（用于"清除" key 场景）
//
// 写法是临时文件 → fsync → rename，原子替换，崩溃不会留下半截配置。
// 临时文件提前 chmod 0600 再 rename，因此最终文件始终是 0600。
func rewriteEnvKey(path, name, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	prefix := name + "="
	found := false
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			found = true
			if value == "" {
				continue // 清除：跳过这一行
			}
			out = append(out, prefix+value)
		} else {
			out = append(out, line)
		}
	}
	if !found && value != "" {
		// 追加时确保前一行有换行结尾
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, prefix+value)
		} else {
			out = append(out, prefix+value, "")
		}
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// 兜底清理：rename 成功后该文件已不存在，Remove 出错可忽略
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(strings.Join(out, "\n")); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	// rename 保留临时文件的权限位，因此 chmod 必须在 rename 之前。
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// MaskKey 把 upstream key 渲染成只露后缀的脱敏字符串，给服务商列表的 masked_key 用。
// 短 key 直接全打码（也不会泄漏长度信号）。
func MaskKey(k string) string {
	if k == "" {
		return ""
	}
	const tailLen = 4
	if len(k) <= tailLen+4 {
		return strings.Repeat("•", len(k))
	}
	return "•••••••••" + k[len(k)-tailLen:]
}
