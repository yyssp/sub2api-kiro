export default {
  cacheStrategies: {
    multiplier: {
      "title": "按比例整形",
      "hint": "直接对上游 usage 各字段按阈值乘倍率，无需会话、缓存命中或冷热判断。仅原值严格大于阈值时，对整个字段相乘并四舍五入；最终仍受模型、请求及用量边界限制。",
      "threshold": "起算阈值（token，严格大于）",
      "factor": "倍率",
      "help": "1 = 不变；1.5 = 放大 50%；0.5 = 缩小 50%。输入与有效输出最终至少为 1，倍率 0 也不会归零；缓存读写允许清零。原始输入为 0 时补齐；原始输出为 0 时按实际内容估算，确实为空不计费。",
      "example": "示例：输出阈值 1,000、倍率 1.5，原输出 1,000 保持不变，1,200 变为 1,800。",
      "enabled": "启用用量倍率",
      "input": "未缓存输入",
      "output": "输出",
      "cache_read": "缓存读",
      "cache_creation": "缓存写",
      "gemini": "Gemini 输出包含回答与思考，两者按原比例分配；生成 usage 没有缓存写，不提供写入倍率。",
      "preview": "以 1,200 token 为例"
},

    audit: {
      partialSave: "策略内容已保存，分组绑定未完成。修正绑定后再次保存会更新同一策略。",
      bindingsLoadFailed: "读取分组绑定失败，已暂停保存以避免清空现有绑定。请关闭后重新编辑。",
    "limits": "最终用量边界",
    "limitsHint": "0 = 自动读取模型目录；未知模型回退为 200k 上下文、64k 输出。输入总量含未缓存输入、缓存读和缓存写，三者共享预算。模型、请求和策略上限取最小值，优先于倍率、采样和成本下限。关闭整形、跳过投影或 Plus 原值模式仍执行零值修复与输入/有效输出至少为 1 的保护。",
    "inputLimit": "输入总量上限（0 = 自动）",
    "outputLimit": "输出上限（0 = 自动）",
    "defaultsHint": "不选模板也已填入完整默认值，可直接保存。高级项用于按需调整；保留上游缓存以顶层开关为准。"
},

    protocols: {
      usage_multiplierHint: "直接对上游 usage 各字段按阈值乘倍率，无需会话、缓存命中或冷热判断。仅原值严格大于阈值时，对整个字段相乘并四舍五入；最终仍受模型、请求及用量边界限制。",
    "uncached_input_max_tokens": "未缓存输入回退上界",
    "reported_input_min_tokens": "输入总量下限（服从最终边界）",
    "final_cache_read_jitter_min_tokens": "读取触顶扣减下限",
    "final_cache_read_jitter_max_tokens": "读取触顶扣减上限",
    "final_output_jitter_min_tokens": "输出触顶扣减下限",
    "final_output_jitter_max_tokens": "输出触顶扣减上限",
    "output_uplift_min_tokens": "输出放大起算阈值",
      prefixHint: "按 systemInstruction、tools 和已结束 contents 的连续前缀匹配；冷请求无缓存读，成功后才更新状态。",
      tool_awareHint: "按原生 parts、functionCall 和 functionResponse 建立更细粒度断点，适合工具往返；保持连续前缀一致性。",
      stable_segment_cacheHint: "按稳定系统、工具与历史边界追踪，支持片段门槛、历史窗口、未缓存输入区间和新增状态上限。",
      prefix_tool_plusHint: "工具前缀追踪后，对未缓存输入、缓存读和输出分别应用倍率、抖动及上下限，不生成缓存写入用量。",
      bounded_high_cacheHint: "先计算已有前缀的读取比例，最后执行输入、读取和输出上限；不使用 Anthropic 的缓存写桶与写入定价。",
      outputGuard: "启用输出最终上限",
      outputUpliftEnabled: "启用输出放大",

      "label": "适用协议",
      "all": "全部协议",
      "hint": "按客户端请求协议执行，与上游账号平台独立。旧策略默认 Anthropic；跨协议请求不套用此策略。",
      "noCreationShort": "生成用量无写入桶",
      "geminiHint": "Gemini：输入为未缓存部分，读取为 cachedContentTokenCount，输出含回答与思考。promptTokenCount = 输入 + 读取。生成响应没有缓存写入 token；显式缓存的存储按现有 token-hours 单独计费，保留真实缓存读取。",
      "usage": "Gemini 用量控制",
      "input": "未缓存输入",
      "cache_read": "缓存读取",
      "output": "输出（含思考）",
      "raw": "原始值",
      "preserve": "保留当前值",
      "sample_max": "上限采样",
      "sample_target": "目标采样",
      "max_tokens": "采样上限（0 不限）",
      "target_tokens": "采样目标",
      "normal_max_multiplier": "目标区间上浮倍数",
      "move_delta": "已有命中时，将缩减的输入转入缓存读",
      "stateHint": "前缀状态写入只影响后续命中，不上报或计费为缓存写入 token。只有成功完成的响应才提交；图片与文件请求保留上游缓存证据。",
      "coverage_ratio": "稳定前缀覆盖比例",
      "read_ratio": "读取倍率",
      "min_cacheable_tokens": "最小缓存前缀 token",
      "uncached_input_min_tokens": "未缓存输入下限",
      "reported_input_max_tokens": "总输入上限（0 不限）",
      "default_ttl_seconds": "前缀 TTL（秒）",
      "state_ttl_seconds": "状态寿命（秒，0 沿用 TTL）",
      "max_coverage_tokens": "前缀覆盖上限（0 不限）",
      "max_new_creation_tokens_per_request": "每次状态新增 token 上限（0 不限）",
      "token_scale": "总输入放大倍率",
      "scale_min_input_tokens": "输入放大起算 token",
      "max_simulated_input_tokens": "模拟总输入上限",
      "max_entries_per_scope": "每个作用域最大条目",
      "max_entries_global": "全局最大条目",
      "estimated_bytes_limit": "状态估算字节上限",
      "expire_after_idle_seconds": "空闲过期（秒）",
      "scope": "缓存作用域",
      "groupSession": "分组 + 会话",
      "accountSession": "分组 + 账号 + 会话",
      "cache_system": "缓存系统指令",
      "cache_tools": "缓存工具定义",
      "cache_history": "缓存已结束历史",
      "cache_tool_results": "缓存工具结果",
      "incremental_create_enabled": "允许增量更新前缀状态",
      "allow_derived_session": "从系统指令、工具和首轮内容派生会话",
      "cold_request_guard": "冷请求禁止合成缓存读并限制输入放大",
      "preserve_upstream_cache_usage": "优先保留上游缓存读",
      "hit": "启用已有命中的读取比例整形",
      "plus": "Plus：三字段独立倍率、抖动与上下限",
      "finalRead": "缓存读最终上限",
      "finalOutput": "输出最终上限",
      "outputUplift": "输出增幅（%）"
},
    advanced: {
      "title": "后续缓存整形能力",
      "stable_segment_cache": "稳定分段缓存",
      "stable_segment_cacheHint": "按稳定 system、tools 和历史消息边界追踪前缀；支持历史窗口、片段门槛、未缓存输入范围和增量写入。",
      "prefix_tool_plus": "前缀追踪 Plus",
      "prefix_tool_plusHint": "四字段独立倍率、浮动与上下限，支持非流式和小请求覆盖；写入频次按放大后的用量计算。",
      "bounded_high_cache": "上下限优先高缓存",
      "bounded_high_cacheHint": "沿用高缓存追踪，最终各字段上下限优先；范围内补不上计费差额时跨成功请求累计。",
      "strictStream": "流式请求严格检查官方缓存资格",
      "strictNonStream": "非流式请求严格检查官方缓存资格",
      "coldGuard": "冷请求读缓存清零，写入限制在真实输入内",
      "stateTTL": "缓存状态寿命（秒，0 沿用旧配置）",
      "eligibilityHint": "严格检查要求客户端缓存标记和模型最小输入长度；不满足时透传 usage。状态寿命与下游展示 5m/1h 分开，最长 300 秒。",
      "hitTitle": "命中率与计费下限",
      "enabled": "启用命中率整形",
      "read_aware_cost_floor": "计费下限计入读取",
      "cost_floor_top_up_input": "剩余计费差额补入输入",
      "target_hit_ratio": "目标命中率（0–0.99）",
      "ratio_jitter": "命中率向上浮动（0–0.2）",
      "min_cache_read_tokens": "非零读取最小值",
      "min_cache_creation_tokens": "非零写入最小值",
      "min_cost_ratio": "最低计费倍数",
      "max_read_multiplier": "读取/原始输入最大倍数（0 不限）",
      "min_creation_ratio": "最小写入/原始输入比例",
      "boundedHint": "优先级：上下限 > 写入频次 > 命中率 > 计费下限。关闭补入输入时，未补足差额直接舍弃。",
      "stableTitle": "稳定分段",
      "min_total_input_tokens": "最小请求输入",
      "min_segment_tokens": "最小稳定前缀",
      "reported_input_min_tokens": "未缓存输入下限",
      "reported_input_max_tokens": "未缓存输入上限（0 不限）",
      "history_message_limit": "历史消息上限（0 不限）",
      "honor_explicit_cache_control": "采信客户端显式缓存标记",
      "auto_cache_system": "自动缓存 system",
      "auto_cache_tools": "自动缓存工具定义",
      "auto_cache_history_message_ends": "自动缓存历史消息边界",
      "plusTitle": "Plus 独立整形",
      "nonStreamMode": "非流式整形",
      "suppressedTarget": "受抑制写入归属",
      "smallEnabled": "启用小请求覆盖",
      "smallMax": "小请求最大真实输入",
      "smallMode": "小请求整形",
      "disableCache": "小请求不显示缓存",
      "inherit": "沿用默认",
      "custom": "独立参数",
      "raw": "保留原始拆分",
      "input": "未缓存输入",
      "output": "输出",
      "cache_read": "缓存读取",
      "cache_creation": "缓存写入",
      "drop": "舍弃",
      "defaultProfile": "默认整形",
      "nonStreamProfile": "非流式独立整形",
      "smallProfile": "小请求独立整形",
      "multiplier": "倍率",
      "jitter_ratio": "±浮动比例",
      "min_tokens": "非零下限",
      "max_tokens": "上限（0 不限）",
      "max_jitter_tokens": "上限扣减幅度",
      "overflowToInput": "缓存超过上限部分转入输入"
},
    title: "缓存策略",
    description:
      "按 Anthropic、Gemini 等客户端协议创建可复用的缓存整形策略，并绑定到分组。",
    create: "新建策略",
    edit: "编辑策略",
    duplicate: "复制策略",
    name: "名称",
    kind: "策略类型",
    templateLabel: "策略模板",
    templateHint:
      "选择模板会立即载入一套可编辑的完整配置，保存后仍可继续调整。",
    usageSummary: "用量整形",
    usageSummaryInput: "输入",
    usageSummaryOutput: "输出",
    usageSummaryRead: "读取",
    usageSummaryCreation: "创建",
    status: "状态",
    boundGroups: "绑定分组",
    revision: "版本",
    actions: "操作",
    enabled: "已启用",
    disabled: "已停用",
    enable: "启用",
    disable: "停用",
    empty: "暂无缓存策略",
    searchPlaceholder: "搜索策略名称或描述",
    allKinds: "全部类型",
    allStatuses: "全部状态",
    refresh: "刷新",
    loadFailed: "加载缓存策略失败",
    saveFailed: "保存缓存策略失败",
    duplicateFailed: "复制缓存策略失败",
    toggleFailed: "更新缓存策略状态失败",
    deleteFailed: "删除缓存策略失败",
    delete: "删除缓存策略",
    deleteConfirm: "确定删除“{name}”吗？已绑定分组的策略需要先解绑。",
    CACHE_STRATEGY_BOUND_TO_GROUPS:
      "该缓存策略仍绑定 {bound_group_count} 个分组，请先解除分组绑定后再删除。",
    CACHE_STRATEGY_GROUP_CONFLICT:
      "分组 {group_name}（ID {group_id}）已经绑定缓存策略 {current_strategy_id}，不能再次绑定到策略 {requested_strategy_id}。请先解除原绑定，再重新绑定。",
    descriptionLabel: "描述",
    descriptionPlaceholder: "说明该策略适用的分组或场景",
    form: {
      basic: "基本信息",
      basicHint: "策略名称和启停状态会展示在策略列表中。",
      usagePolicy: "用量上报策略",
      usagePolicyHint:
        "这里控制返回给 Claude Code 的 input、output、cache read、cache creation 四个数值。它只整理用量，不改变实际发送给上游的请求内容。",
      usageFlow:
        "计算顺序：先读取上游用量，再合并本地缓存命中/创建证据，最后按下方四项策略整形。input 被压低的差值会按设置归入 cache read（有命中证据）或 cache creation（无命中证据），不会凭空丢失。",
      usageExample:
        "示例：上游总输入 10,000，命中缓存 7,000，新建缓存 2,000，则 Claude 响应中的 input=1,000、cache_read=7,000、cache_creation=2,000；OpenAI 响应中的总 input=10,000。",
      usageInput: "输入 Token（input_tokens）",
      usageInputHint:
        "Claude 协议中表示未命中缓存的输入部分；OpenAI 兼容协议会将它与 cache read/create 合并为总输入。",
      usageOutput: "输出 Token（output_tokens）",
      usageOutputHint:
        "控制响应中的输出 Token。可按原值、最大值或目标值上报，并可设置阈值放大和最终上限。",
      usageCacheRead: "读取缓存（cache_read_input_tokens）",
      usageCacheReadHint:
        "控制已经命中稳定前缀的 Token 数。它来自本地缓存证据或上游明确返回的 cached tokens。",
      usageCacheCreation: "创建缓存（cache_creation_input_tokens）",
      usageCacheCreationHint:
        "控制本次新写入稳定前缀的 Token 数。创建频率和预算仍由下方 Creation 控制负责。",
      usageMode: "上报模式",
      moveInputDelta: "输入减少的差值归入缓存字段",
      usageMaxTokens: "最大 Token",
      usageTargetTokens: "目标 Token",
      usageNormalMaxMultiplier: "目标常规最大倍率",
      outputUpliftEnabled: "启用输出放大",
      outputUpliftEnabledHint:
        "关闭后阈值与比例都不生效，数值会原样保留，重新开启不用再填一遍。",
      outputUpliftMinTokens: "输出放大阈值",
      outputUpliftPercent: "输出放大比例（%）",
      finalOutputGuardEnabled: "启用输出上限（放大与上限的总开关）",
      finalOutputGuardEnabledHint:
        "关掉后输出放大与输出上限都不生效，数值保留不用清零。未配置时默认开启。",
      finalOutputMaxTokens: "输出上限（0=关闭）",
      finalOutputJitterMinTokens: "上限波动区间下限",
      finalOutputJitterMaxTokens: "上限波动区间上限",
      finalCacheReadMaxTokens: "缓存读取上限（0=关闭）",
      finalCacheReadJitterMinTokens: "上限波动区间下限",
      finalCacheReadJitterMaxTokens: "上限波动区间上限",
      finalCacheCreationMaxTokens: "缓存写入上限（0=关闭）",
      finalCacheCreationJitterMinTokens: "上限波动区间下限",
      finalCacheCreationJitterMaxTokens: "上限波动区间上限",
      finalCapJitterHint:
        "触顶时按请求指纹在这个区间内确定性回退一点，避免所有触顶记录都显示同一个数字；同一请求重试结果保持一致。波动下限应显著小于上限本身，且不能把上限扣减到 0。小上限配置会按比例缩放区间；上限为 0（不限制）时该区间自动清零。",
      finalCapJitterValidationHint:
        "建议：波动下限至少比上限低一个明显区间，波动上限必须小于上限；否则归一化会缩放配置，触顶值的波动范围会变窄。",
      finalCapJitterValidationError:
        "上限波动区间无效：波动上限必须大于下限，且波动下限必须小于最终上限；最终上限为 1 时请关闭波动。",
      skipNonStreamUsageProjection: "非流式响应跳过用量投影",
      skipNonStreamUsageProjectionHint:
        "非流式响应能拿到完整的上游 usage，勾选后原样透传、不套缓存整形。仅影响非流式，流式响应不受影响。",
      behavior: "缓存命中与创建行为（高级）",
      behaviorHint:
        "这些参数决定本地缓存证据如何产生；它们不是最终用量上报值。最终返回的 input/output/read/create 请在上方用量上报策略中设置。",
      limits: "缓存资源与生命周期（高级）",
      limitsHint:
        "控制缓存容量、Token 上限和过期时间；不会替代上方四项用量策略。",
      segments: "缓存内容范围（高级）",
      segmentsHint: "选择哪些请求内容可以进入稳定缓存前缀。",
      creationControl: "创建频率控制（高级）",
      creationControlHint:
        "限制连续请求中创建缓存的频率和预算；它只决定是否允许创建，不改变读取缓存。",
      bindings: "绑定分组",
      bindingsHint: "同一个策略可以绑定多个分组；分组只使用绑定的通用策略。",
      cacheNamespaceHint:
        "缓存默认按分组、会话、策略版本和协议隔离（作用域「分组 + 会话」）：同一会话切换账号仍可命中，不必重建前缀。改成「分组 + 账号 + 会话」后会额外按账号隔离，换账号即重新创建缓存。",
      coverageRatio: "缓存证据覆盖比例",
      coverageRatioHint:
        "实际可纳入缓存的稳定前缀比例；越低会同时减少后续可读和可写的基础量。",
      usageRatio: "缓存证据总比例",
      usageRatioHint:
        "统一缩放对外 cache read/create evidence；主要影响 usage 数值，不等同于实际缓存容量。",
      ratioMode: "缓存证据比例模式",
      ratioModeHint:
        "统一比例同时作用于读写；独立比例允许分别控制 read 和 creation 的上报量。",
      readRatio: "读取证据比例",
      readRatioHint:
        "仅独立比例模式生效；越低，对外 cache_read 数值越小，但不会删除已有缓存。",
      creationRatio: "创建证据比例",
      creationRatioHint:
        "仅独立比例模式生效；越低，对外 cache_creation 数值越小，不是实际写入硬上限。",
      breakpointMode: "断点模式",
      breakpointModeHint:
        "决定使用客户端断点、自动断点还是混合断点；没有可用断点时不会产生缓存读写。",
      minCacheableTokens: "最小可缓存 Token",
      minCacheableTokensHint:
        "⚠️ 该值过高会让整条缓存策略失效。比较的是「断点处的累计前缀 Token」而非请求总量；" +
        "当所有断点都低于该值时，整个缓存档案会被丢弃，该请求完全不产生 cache_read / cache_creation。" +
        "填 0（默认）表示不设门槛，所有断点都可缓存。" +
        "它只做过滤，不会把小请求向上补到这个数值。",
      maxCoverageTokens: "最大覆盖 Token",
      maxCoverageTokensHint:
        "实际缓存前缀的绝对上限；达到后 read 可以继续命中，但新的 creation 会逐渐变少或为 0。",
      maxCreationTokens: "单次最大 Creation Token",
      maxCreationTokensHint:
        "单请求真实新增缓存前缀的硬上限；与 usage 区域的最终写入上限不同。",
      defaultTtlSeconds: "默认 TTL（秒）",
      defaultTtlSecondsHint:
        "普通缓存断点的生命周期；过期后下一次需要重新创建，TTL 不是 usage 上限。",
      hourTtlSeconds: "1 小时 TTL（秒）",
      hourTtlSecondsHint:
        "标记为 1 小时的断点生命周期；必须不小于默认 TTL，最长受系统支持范围限制。",
      forcedTtlTier: "强制上报档位",
      forcedTtlTierHint:
        "上报档位按「强制档位 → 上游返回 → 客户端声明 → 5m」四级优先级决定，本项是最高级。选择「跟随」即不强制；选 5m 或 1h 则无视上游与客户端，一律按该档上报。只影响上报给客户的计费分桶，不改变上游实际成本。",
      forcedTtlTierFollow: "跟随（不强制）",
      forcedTtlTier5m: "强制 5 分钟",
      forcedTtlTier1h: "强制 1 小时",
      trustUpstreamTtlTier: "采信上游档位",
      trustUpstreamTtlTierHint:
        "开启后，上游响应实际返回哪个档位就按哪个上报，使收入口径与成本口径对齐。三方接入若不遵守客户端声明（客户端要 5m、上游按 1h 计费），关闭本项会导致差价由平台承担。上游未返回档位时自动退回客户端声明。",
      tokenScale: "Token 缩放系数",
      tokenScaleHint:
        "长输入达到阈值后用于 usage 模拟的放大系数，不改变发送给上游的 prompt。",
      scaleMinInputTokens: "启用缩放的最小输入 Token",
      scaleMinInputTokensHint:
        "输入达到该值后才启用 token 缩放；短请求不会被强行放大。",
      maxSimulatedInputTokens: "最大模拟输入 Token",
      maxSimulatedInputTokensHint:
        "模拟 usage 总输入上限；大窗口模板需要足够大，否则读写合计会提前被压缩。",
      capJitterMinTokens: "输入上限抖动下限",
      capJitterMinTokensHint:
        "模拟 input 触顶时向下扣减的最小值；只有真正触顶才生效。",
      capJitterMaxTokens: "输入上限抖动上限",
      capJitterMaxTokensHint:
        "模拟 input 触顶时向下扣减的最大值；应小于上限，避免触顶值被压得过低。",
      maxEntriesPerScope: "单作用域最大条目数",
      maxEntriesPerScopeHint:
        "单个分组/会话 scope 的缓存条目数量；超出后淘汰旧条目，可能降低后续 read。",
      maxEntriesGlobal: "全局最大条目数",
      maxEntriesGlobalHint:
        "所有 scope 合计的缓存条目上限；超出后全局淘汰旧条目。",
      estimatedBytesLimit: "缓存估算字节上限",
      estimatedBytesLimitHint:
        "按估算内存控制缓存总容量；达到上限会淘汰旧条目，不改变单次 usage 算法。",
      expireAfterIdleSeconds: "空闲过期时间（秒）",
      expireAfterIdleSecondsHint:
        "条目连续空闲超过该时间后删除；长时间不访问的会话恢复时通常需要重写。",
      currentUserStablePrefix: "缓存当前用户稳定前缀",
      currentUserStablePrefixHint:
        "是否把当前用户消息的稳定前缀加入缓存；开启可提高大上下文写入/下一轮读取，但更容易缓存动态内容。",
      currentUserStablePrefixMaxTokens: "用户稳定前缀最大 Token",
      currentUserStablePrefixMaxTokensHint:
        "当前用户稳定前缀最多缓存的 Token；只在上方开关开启时生效。",
      scopeMode: "缓存作用域",
      scopeModeHint:
        "分组+会话允许同会话跨账号命中；加入账号后换账号会重新创建缓存。",
      dynamicContentMode: "动态内容处理",
      dynamicContentModeHint:
        "排除动态字段通常更稳定；允许动态内容可能降低命中率并增加重复 creation。",
      allowDerivedSession: "允许从请求派生会话",
      allowDerivedSessionHint:
        "请求缺少 session id 时是否生成派生 scope；开启能覆盖更多请求，但会放宽会话边界。",
      preserveUpstreamCacheUsage: "优先保留上游缓存 usage",
      incrementalCreation: "启用增量 Creation",
      incrementalCreationHint:
        "命中已有缓存后是否继续为新增稳定前缀写入；关闭只停止新增，不会删除已有 read。",
      cacheSystem: "缓存 System",
      cacheSystemHint:
        "System 内容会进入稳定前缀；内容越大，后续缓存读写的基础量通常越高。",
      cacheTools: "缓存 Tools",
      cacheToolsHint:
        "Tools 定义会进入稳定前缀；工具定义稳定时更容易命中，频繁变化时会降低前缀稳定性。",
      cacheHistory: "缓存历史消息",
      cacheHistoryHint:
        "历史消息会进入稳定前缀；长会话中的 cache_read 通常主要来自这一部分。",
      cacheToolResults: "缓存 Tool Results",
      cacheToolResultsHint:
        "工具结果会进入稳定前缀；如果结果包含动态 ID、时间戳等字段，重复 creation 的概率会更高。",
      creationControlEnabled: "启用 Creation 限流",
      creationControlEnabledHint:
        "控制 creation 的出现频次、单次显示量和窗口预算；不限制已有 cache_read。",
      minCreationDeltaTokens: "最小 Creation 增量 Token",
      minCreationDeltaTokensHint:
        "低于该增量时暂缓释放，后续请求可累积后再释放；越大通常写入频次越低。",
      minSuccessfulRequestsBetween: "两次 Creation 间成功请求数",
      minSuccessfulRequestsBetweenHint:
        "两次 creation 之间必须经过的成功请求数；中间请求仍可以命中 read。",
      minCreationIntervalSeconds: "Creation 最小间隔（秒）",
      minCreationIntervalSecondsHint:
        "两次 creation 的最短时间间隔；连发请求越容易被压制。",
      maxCreationTokensPerEvent: "单次事件最大 Creation Token",
      maxCreationTokensPerEventHint:
        "单次 creation 对外可见的最大值；需要限制真实新增时还要设置单次最大 Creation Token。",
      creationBudgetWindowSeconds: "Creation 预算窗口（秒）",
      creationBudgetWindowSecondsHint:
        "统计窗口 creation 预算的时间长度；窗口结束后预算重新计算。",
      maxCreationTokensPerWindow: "窗口最大 Creation Token",
      maxCreationTokensPerWindowHint:
        "窗口内允许对外释放的 creation 总量；用尽后 creation 可以为 0，但已有 read 不会消失。",
    },
    kinds: {
      prefix: "前缀缓存",
      toolAware: "工具感知缓存",
      disabled: "关闭缓存",
    },
    kindDescriptions: {
      prefix: "按稳定请求前缀建立缓存，适合大多数 Claude Code 兼容分组。",
      toolAware:
        "在稳定前缀基础上细分工具、历史和工具结果，适合工具密集型长会话。",
      disabled: "不读、不写本地缓存，也不补足模拟的 cache usage。",
    },
    templates: {
      blank: {
        name: "自定义空白策略",
        description: "从默认配置开始，自行调整缓存范围、读写比例、usage 整形和创建频控。",
      },
      kiroRsTool: {
        name: "kiro-rs-tool",
        description:
          "覆盖率 100%、不放大 token、不限制创建频率，上报值贴近真实前缀命中。未缓存输入固定在 32~4096，适合以工具调用为主的 Claude Code 会话。",
      },
      highCache: {
        name: "高缓存（默认）",
        description:
          "通用 Claude Code 兼容协议的高缓存基线：稳定前缀、98% 证据比例、长输入缩放和最终 usage 上限。单次创建上限 100k，适合日常生产流量。",
      },
      steadyGrowth: {
        name: "稳步增长",
        description:
          "每轮写入受 100k 单次上限约束，缓存呈平稳上升。关闭最小间隔、成功次数和窗口预算限制，适合观察缓存逐轮增长。",
      },
      rapidGrowth: {
        name: "快速增长",
        description:
          "几轮内冲到较大数值，且每轮增量不规整。单次创建上限 120k、窗口预算 2M，适合压测高吞吐会话。",
      },
      largeWindow: {
        name: "大数值读写（700k 读 / 500k 写）",
        description:
          "读写都允许进入大数值区间：缓存读取最终上限 700k、写入最终上限 500k，写入保留原始值并关闭创建频控。适合大上下文和边界压测，不建议直接作为低吞吐生产默认。",
      },
      largeReadControlledWrite: {
        name: "大读可控写（700k 读 / 120k 目标写）",
        description:
          "缓存读取保留 700k 能力；写入按约 120k 目标、180k 上限整形，并限制创建释放频率。适合希望高命中、但不希望每轮写入占满 usage 的会话。",
      },
      largeReadSmallWrite: {
        name: "大读小写（700k 读 / 约 30k 写）",
        description:
          "缓存读取可到 700k；写入以约 30k 为目标、60k 为上限，并保留较小窗口预算。适合需要高缓存命中、但要严格控制写入成本或写入频次的分组。",
      },
      largeWriteControlledRead: {
        name: "大写可控读（500k 写 / 250k 读）",
        description:
          "缓存写入保留 500k 能力；cache_read 最终上报限制为 250k。读取限制只影响对外 usage，不会删除 tracker 中已建立的缓存前缀。",
      },
    },
    ratioModes: {
      uniform: "统一比例",
      independent: "读写独立比例",
    },
    usageModes: {
      raw: "原样上报（上游值）",
      preserve: "保留计算值（不再整形）",
      sampleMax: "限制最大值（不超过上限）",
      sampleTarget: "按目标值采样（确定性）",
      rawShort: "原样",
      preserveShort: "保留",
      sampleMaxShort: "最大值",
      sampleTargetShort: "目标值",
    },
    breakpointModes: {
      clientOnly: "仅客户端断点",
      hybrid: "混合断点",
      auto: "自动断点",
    },
    scopeModes: {
      groupAccountSession: "分组 + 账号 + 会话",
      groupSession: "分组 + 会话（跨账号共享）",
    },
    dynamicContentModes: {
      exclude: "排除动态内容",
      allow: "允许动态内容（高风险）",
    },
    currentSelection: "已选择 {count} 个分组",
    noGroups: "暂无可绑定分组",
    copyName: "{name} 副本",
    copySuccess: "缓存策略已复制",
    saveSuccess: "缓存策略已保存",
    toggleSuccess: "缓存策略状态已更新",
    deleteSuccess: "缓存策略已删除",
    unbound: "未绑定分组",
    groupCount: "{count} 个分组",
  },
};
