export default {
  cacheStrategies: {
    title: "缓存策略",
    description:
      "为 Claude Code 兼容协议创建可复用的缓存整形策略，并绑定到分组。",
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
        "小于该值的断点不会进入 tracker；它不会把小请求向上补到这个数值。",
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
