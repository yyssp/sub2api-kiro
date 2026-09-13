package cursor

import "time"

// Account 是协议层所需的 Cursor 账号视图——**只含凭证与身份**，不含任何运行时状态。
//
// 它刻意不是 ai2api 的 CursorAccount 全量搬迁：原 struct 有 40+ 字段，
// 把凭证、官方面板用量、冷却/失败计数、套餐能力记忆混在一起。协议层
// 实际只用到下面这些字段（由 ai2api 调用点统计得出）。
//
// ⚠️ 不要往本 struct 加运行时状态（冷却至、连续失败数、最后成功时间）。
// 那些在 sub2api 侧一律用 accounts 表既有列（rate_limited_at /
// temp_unschedulable_until / status / error_message），由业务层负责。
// 协议层保持无状态，才能独立单测、且不碰 DB/Redis。
//
// 三桶额度（cursor/other/grokbot）的**当前快照**可以填在这里供能力判定，
// 但真相源在业务层的 extra["cursor_quota"]（见 cursor_http_helpers.go 的
// readCursorQuota/writeCursorQuota）。协议层只读不写这些字段。
type Account struct {
	// 身份
	ID    int64  // 仅用于日志/追踪关联，对应 sub2api 的 accounts.id
	Email string // 可选，日志脱敏展示用

	// 凭证
	AccessToken   string    // 纯 JWT(api2.cursor.sh Bearer)
	RefreshToken  string    // deep-login 兑换所得，备用
	Session       string    // uid::JWT(cursor.com 面板 cookie 值)
	MachineID     string    // 设备指纹种子，参与 x-cursor-checksum 计算
	AccountExpiry time.Time // token JWT exp，用于判断是否需刷新

	// 调度/能力判定所需的最小状态（由业务层从 DB/extra 填入，协议层只读）
	Disabled   bool   // 当前是否不可用于常规请求面
	Membership string // 套餐，决定 Sand/GrokBot 能力探测策略

	// ProxyURL 是该账号绑定的出站代理（由业务层从 accounts.proxy_id 关联填入）。
	// 空串表示直连（退回 http.ProxyFromEnvironment）。
	//
	// ⚠️ Cursor 按设备指纹 + 出口 IP 做风控：同一批账号从同一出口 IP 打过去
	// 容易被整体判定异常。账号级代理是把号池分散到不同出口的唯一手段，
	// 漏接会让管理端配好的代理对 Cursor 静默失效（其它平台都生效，只有它不）。
	ProxyURL string

	// ── 三桶额度快照（由业务层从 extra["cursor_quota"] 填入，协议层只读）──
	// ⚠️ 单桶耗尽 ≠ 账号不可用：一个账号可能 cursor 桶耗尽但 grokbot 桶可用。
	// 调度时必须按「目标模型属于哪个桶」判断，见 ModelToQuotaBucket。

	CursorModelsPct float64   // cursor 桶（auto/composer/vega/grok）使用率
	OtherModelsPct  float64   // other 桶（命名第三方模型）使用率
	UsageAt         time.Time // 上述百分比的抓取时间；零值表示从未抓取，不可当作 0% 可用

	// 对应两个百分比的抓取状态，语义同 GrokBotState。
	// ⚠️ 百分比本身无法区分「真的 0% 已用」和「这次没读到」：抓取失败的桶
	// Percent 保持零值，只看 Pct<100 会把它判成 100% 空闲，甚至复活刚被标记
	// 耗尽的桶。空串按已读到处理（兼容未填状态的旧调用方）。
	CursorModelsState string
	OtherModelsState  string

	// GrokBot 桶状态：五态区分「字段未返回」与「确认耗尽」，
	// 不能只看 GrokBotEnabled 做调度（见 entitlement.go 的说明）。
	GrokBotState   string
	GrokBotPct     float64
	GrokBotEnabled bool

	// NamedModelsUnavailable 记忆上游套餐能力：Free 账号收到
	// "Named models unavailable - Free plans can only use Auto" 后，
	// 命名模型不再重复尝试，Auto/Cursor 自有模型仍可用。
	NamedModelsUnavailable bool

	// LastError 是业务层从 accounts.error_message 填入的最近错误文案，
	// 仅用于 accountCredentialUnavailable 判定；协议层不写它。
	LastError string
}
