// config.go 加载 JSON 配置 + 环境变量覆盖。
package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "60s"
	} `json:"cooldown"`

	// UpstreamRotate 请求级轮转（换号重试）配置。
	UpstreamRotate struct {
		// MaxRotate 单请求最多尝试的账号数（含首次）。
		// 0/缺省 = 默认 5；1 = 不轮转（把上游错误直接暴露给客户端）。
		// 实测（2026-09-13）：21 账号池里原本硬编码的 3 次会被已死账号耗光，
		// 返回 503 而 8 秒后就成功——说明可用账号一直存在，只是抽样次数不够。
		MaxRotate int `json:"max_rotate"`
	} `json:"upstream_rotate"`

	Schedule struct {
		CheckinHours   []int `json:"checkin_hours"`   // [9,21]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		// CreditRefresh 额度刷新间隔（duration 字符串，如 "30m"）。
		// 空/非法/<=0 表示关闭定时刷新（仅保留签到与手动刷新）。
		// 上游 billing 接口对频率敏感（项目自带 credit 工具都串行加 200ms 间隔），
		// 故默认 30m：一天 48 次/账号，足以反映签到后的余额变化，限流风险低。
		CreditRefresh string `json:"credit_refresh"`
	} `json:"schedule"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
		// ImageFootnote 生图（hunyuan 族）右下角水印控制：
		// 上游 footnote 即水印文案字段（≤16 字符）。三态：
		//   缺键 → 不发 footnote（上游加默认水印，现状）；
		//   false → 生图默认发 footnote:""（试探上游以空串关水印，未确认生效）；
		//   true  → 生图默认发 image_footnote_text（空 text 与 false 等效）。
		// 客户端请求显式传 footnote 时优先于本默认。
		ImageFootnote     *bool  `json:"image_footnote"`
		ImageFootnoteText string `json:"image_footnote_text"`
	} `json:"features"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int     `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		BreakerThreshold   int     `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string  `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string  `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
	} `json:"pool"`

	// Proxy 账号级出口代理（每个账号走一个独立本地端口 = 独立出口 IP）。
	//
	// 默认行为（用户要求，2026-09-13）：启动时【自动开启】，不需要点开关、
	// 也不需要在这里写任何配置。因此 Auto 的默认值必须为 true ——
	// 零值（缺键）与显式 false 无法区分，用指针表达三态：
	//
	//	缺键   → Auto=true（自动开启，默认）
	//	false  → 不自动开启（用户明确要求改配置才行）
	//	true   → 自动开启（等同缺键）
	//
	// 运行期间点「关闭代理」只在内存生效：本次运行保持直连，重启后又恢复自动开启。
	Proxy struct {
		// Auto 启动时自动开启代理（默认 true）。
		Auto *bool `json:"auto"`
		// Enabled 旧键（config 驱动启用）。保留兼容：true 等价于 auto=true。
		Enabled bool `json:"enabled"`
		// ClashAPI 本地 Clash 外部控制地址（默认 127.0.0.1:9097）。
		ClashAPI string `json:"clash_api"`
		// ClashSecret Clash API 的 secret（Verge 里配置的那个）。
		ClashSecret string `json:"clash_secret"`
		// PortBase listeners 起始端口（默认 34567，连续分配）。
		PortBase int `json:"port_base"`
		// BindingsFile 绑定持久化路径（默认 data/proxy-bindings.json）。
		// 必须持久化：同一账号的出口 IP 要稳定，重启后不能换；
		// 同时它也是"上次对应关系"的来源，失效时才重新分配。
		BindingsFile string `json:"bindings_file"`
		// StateFile 接管等级持久化路径（默认 data/proxy-state.json）。
		// 记录上次是"网关写 listeners"还是"沿用用户已配置的 listeners"。
		StateFile string `json:"state_file"`
		// HealthInterval 健康探测周期（duration 字符串，默认 5m）。
		HealthInterval string `json:"health_interval"`
	} `json:"proxy"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	// CreditRefreshDur 解析后的额度刷新间隔；0 表示关闭定时刷新。
	CreditRefreshDur time.Duration `json:"-"`

	// HealthIntervalDur 解析后的代理健康探测周期。
	HealthIntervalDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "60s"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.CreditRefresh = "30m"
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	c.Features.SanitizeBlacklistFingerprints = true
	c.Pool.MaxInFlight = 3
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_IMAGE_FOOTNOTE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.ImageFootnote = &b
		}
	}
	if v := os.Getenv("WB2A_IMAGE_FOOTNOTE_TEXT"); v != "" {
		c.Features.ImageFootnoteText = v
	}
}

func (c *Config) normalize() error {
	var err error
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// 额度刷新：空串视为未设置 → 用默认 30m；显式 "0" / "0s" → 关闭定时刷新。
	// 其余非法格式报错，避免用户写了错值却以为已生效。
	if c.Schedule.CreditRefresh == "" {
		c.Schedule.CreditRefresh = "30m"
	}
	if c.CreditRefreshDur, err = time.ParseDuration(c.Schedule.CreditRefresh); err != nil {
		return fmt.Errorf("schedule.credit_refresh: %w", err)
	}
	// 代理默认值：无论是否启用都要填好——启动自动开启是本项目的默认行为，
	// 用户不该为了"能用"而先写配置（这正是本功能被要求的初衷）。
	if c.Proxy.ClashAPI == "" {
		c.Proxy.ClashAPI = "http://127.0.0.1:9097"
	}
	if c.Proxy.PortBase <= 0 {
		c.Proxy.PortBase = 34567
	}
	if c.Proxy.BindingsFile == "" {
		c.Proxy.BindingsFile = filepath.Join(filepath.Dir(c.StateFile), "proxy-bindings.json")
	}
	if c.Proxy.StateFile == "" {
		c.Proxy.StateFile = filepath.Join(filepath.Dir(c.StateFile), "proxy-state.json")
	}
	if c.Proxy.HealthInterval == "" {
		c.Proxy.HealthInterval = "5m"
	}
	if c.HealthIntervalDur, err = time.ParseDuration(c.Proxy.HealthInterval); err != nil {
		return fmt.Errorf("proxy.health_interval: %w", err)
	}
	if c.HealthIntervalDur <= 0 {
		c.HealthIntervalDur = 5 * time.Minute
	}
	// 旧键 proxy.enabled=true 等价于自动开启（保持既有配置的兼容行为）。
	if c.Proxy.Enabled && c.Proxy.Auto == nil {
		on := true
		c.Proxy.Auto = &on
	}
	if c.CreditRefreshDur < 0 {
		c.CreditRefreshDur = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	return nil
}

// ProxyAuto 报告"启动时是否应自动开启代理"。
//
// 三态语义（见 Config.Proxy.Auto 的注释）：
//   - 缺键（nil）→ true：默认自动开启，用户不需要改配置；
//   - 显式 false → false：不自动开启；
//   - 显式 true → true。
//
// 旧键 proxy.enabled=true 也归一化为 true（已在上面的 normalize 里处理）。
func (c *Config) ProxyAuto() bool {
	if c == nil {
		return false
	}
	if c.Proxy.Auto != nil {
		return *c.Proxy.Auto
	}
	return true
}

// ImageFootnoteDefault 返回生图默认 footnote 注入值。
// nil = 不注入（features.image_footnote 缺键）；非 nil 为待发送的水印文案：
// false → ""（显式空串，试探关水印）；true → image_footnote_text。
func (c *Config) ImageFootnoteDefault() *string {
	if c == nil || c.Features.ImageFootnote == nil {
		return nil
	}
	if !*c.Features.ImageFootnote {
		s := ""
		return &s
	}
	s := c.Features.ImageFootnoteText
	return &s
}
