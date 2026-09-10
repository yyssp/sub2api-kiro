export default {
  cacheStrategies: {
    title: "Cache Strategies",
    description:
      "Create reusable cache shaping policies for Claude Code-compatible protocols and bind them to groups.",
    create: "Create strategy",
    edit: "Edit strategy",
    duplicate: "Duplicate strategy",
    name: "Name",
    kind: "Strategy type",
    templateLabel: "Strategy template",
    templateHint:
      "Selecting a template immediately loads an editable configuration. You can fine-tune it before saving.",
    usageSummary: "Usage shaping",
    usageSummaryInput: "Input",
    usageSummaryOutput: "Output",
    usageSummaryRead: "Read",
    usageSummaryCreation: "Create",
    status: "Status",
    boundGroups: "Bound groups",
    revision: "Revision",
    actions: "Actions",
    enabled: "Enabled",
    disabled: "Disabled",
    enable: "Enable",
    disable: "Disable",
    empty: "No cache strategies",
    searchPlaceholder: "Search strategy name or description",
    allKinds: "All types",
    allStatuses: "All statuses",
    refresh: "Refresh",
    loadFailed: "Failed to load cache strategies",
    saveFailed: "Failed to save cache strategy",
    duplicateFailed: "Failed to duplicate cache strategy",
    toggleFailed: "Failed to update cache strategy status",
    deleteFailed: "Failed to delete cache strategy",
    delete: "Delete cache strategy",
    deleteConfirm: "Delete “{name}”? Unbind its groups before deleting it.",
    CACHE_STRATEGY_GROUP_CONFLICT:
      "Group {group_name} (ID {group_id}) is already bound to cache strategy {current_strategy_id} and cannot be rebound to strategy {requested_strategy_id}. {reason}",
    descriptionLabel: "Description",
    descriptionPlaceholder:
      "Describe the groups or scenarios this policy is for",
    form: {
      basic: "Basic information",
      basicHint: "The name and status are shown in the strategy list.",
      usagePolicy: "Usage reporting policy",
      usagePolicyHint:
        "Controls the four values returned to Claude Code: input, output, cache read, and cache creation. It shapes usage only and does not change the request sent upstream.",
      usageFlow:
        "Order: read upstream usage, merge local cache hit/creation evidence, then apply the four policies below. When input is reduced, the delta is assigned to cache read when hit evidence exists or cache creation otherwise.",
      usageExample:
        "Example: total input 10,000 with 7,000 cache-read and 2,000 cache-creation tokens produces Claude input=1,000, cache_read=7,000, cache_creation=2,000; OpenAI reports total input=10,000.",
      usageInput: "Input tokens (input_tokens)",
      usageInputHint:
        "The uncached input portion for Claude. OpenAI-compatible responses add cache read/create back into the total input.",
      usageOutput: "Output tokens (output_tokens)",
      usageOutputHint:
        "Controls output tokens returned in the response, with raw, maximum, or target-based reporting plus optional uplift and final cap.",
      usageCacheRead: "Cache read (cache_read_input_tokens)",
      usageCacheReadHint:
        "Tokens served from an already cached stable prefix, based on local evidence or upstream cached-token metadata.",
      usageCacheCreation: "Cache creation (cache_creation_input_tokens)",
      usageCacheCreationHint:
        "Tokens written into the stable prefix during this request. Creation frequency and budget remain controlled below.",
      usageMode: "Reporting mode",
      moveInputDelta: "Move reduced input delta into cache usage",
      usageMaxTokens: "Maximum tokens",
      usageTargetTokens: "Target tokens",
      usageNormalMaxMultiplier: "Normal maximum multiplier",
      outputUpliftMinTokens: "Output uplift threshold (0=off)",
      outputUpliftPercent: "Output uplift percent",
      finalOutputMaxTokens: "Final output cap (0=off)",
      finalCacheReadMaxTokens: "Final cache-read cap (0=off)",
      finalCacheCreationMaxTokens: "Final cache-creation cap (0=off)",
      behavior: "Cache hit and creation behavior (advanced)",
      behaviorHint:
        "These settings control how local cache evidence is produced, not the final usage values. Set input/output/read/create reporting in the Usage reporting policy section above.",
      limits: "Cache resources and lifecycle (advanced)",
      limitsHint:
        "Controls cache capacity, token limits, and expiration; it does not replace the four usage policies above.",
      segments: "Cache content scope (advanced)",
      segmentsHint:
        "Choose which request content can enter the stable cache prefix.",
      creationControl: "Creation frequency controls (advanced)",
      creationControlHint:
        "Limits when new cache entries may be created; it does not change cache reads.",
      bindings: "Group bindings",
      bindingsHint:
        "One strategy can be bound to multiple groups; groups use the shared policy.",
      cacheNamespaceHint:
        "Cache namespaces are isolated by account, group, strategy revision, and protocol. Switching to another account starts a new cache instead of reusing the previous account's entries.",
      coverageRatio: "Cache-evidence coverage ratio",
      usageRatio: "Cache-evidence total ratio",
      ratioMode: "Cache-evidence ratio mode",
      readRatio: "Read-evidence ratio",
      creationRatio: "Creation-evidence ratio",
      breakpointMode: "Breakpoint mode",
      minCacheableTokens: "Minimum cacheable tokens",
      maxCoverageTokens: "Maximum coverage tokens",
      maxCreationTokens: "Maximum creation tokens per request",
      defaultTtlSeconds: "Default TTL (seconds)",
      hourTtlSeconds: "1-hour TTL (seconds)",
      tokenScale: "Token scale",
      scaleMinInputTokens: "Minimum input tokens for scaling",
      maxSimulatedInputTokens: "Maximum simulated input tokens",
      capJitterMinTokens: "Minimum cap jitter tokens",
      capJitterMaxTokens: "Maximum cap jitter tokens",
      maxEntriesPerScope: "Maximum entries per scope",
      maxEntriesGlobal: "Maximum global entries",
      estimatedBytesLimit: "Estimated cache byte limit",
      expireAfterIdleSeconds: "Idle expiration (seconds)",
      currentUserStablePrefix: "Cache current-user stable prefix",
      currentUserStablePrefixMaxTokens: "Maximum user-prefix tokens",
      scopeMode: "Cache scope",
      dynamicContentMode: "Dynamic content handling",
      allowDerivedSession: "Allow derived sessions",
      preserveUpstreamCacheUsage: "Prefer upstream cache usage",
      incrementalCreation: "Enable incremental creation",
      cacheSystem: "Cache system",
      cacheTools: "Cache tools",
      cacheHistory: "Cache message history",
      cacheToolResults: "Cache tool results",
      creationControlEnabled: "Enable creation throttling",
      minCreationDeltaTokens: "Minimum creation delta tokens",
      minSuccessfulRequestsBetween: "Successful requests between creations",
      minCreationIntervalSeconds: "Minimum creation interval (seconds)",
      maxCreationTokensPerEvent: "Maximum creation tokens per event",
      creationBudgetWindowSeconds: "Creation budget window (seconds)",
      maxCreationTokensPerWindow: "Maximum creation tokens per window",
    },
    kinds: {
      prefix: "Prefix cache",
      toolAware: "Tool-aware cache",
      disabled: "Cache disabled",
    },
    kindDescriptions: {
      prefix:
        "Builds cache entries from stable request prefixes for most Claude Code-compatible groups.",
      toolAware:
        "Refines stable prefixes around tools, history, and tool results for tool-heavy long sessions.",
      disabled:
        "Disables local cache reads, writes, and simulated cache usage completion.",
    },
    templates: {
      blank: {
        name: "Custom blank strategy",
      },
      highCache: {
        name: "High cache (default)",
        description:
          "Maps to the reference project's default high-cache route: stable prefixes, 98% usage ratio, long-input token scaling, and final usage caps.",
      },
      claudeCode: {
        name: "Claude Code tool session",
        description:
          "Maps to the Claude Code high-cache route: tool-aware prefixes, input shaped to 96 tokens with the delta moved to cache read, and creation centered around 3,000 tokens.",
      },
      inputShaping: {
        name: "Input shaping (high cache)",
        description:
          "Maps to the input-only high-cache route: preserve calculated cache read/write values while capping input at 96 tokens and moving the delta to cache read.",
      },
      lowFrequencyCreation: {
        name: "Low-frequency creation",
        description:
          "Keeps existing prefix reads while slowing new writes: the first valid creation is allowed, then a success-count gate, per-event cap, and five-minute budget apply.",
      },
      readPriority: {
        name: "Read priority",
        description:
          "Prefers tool-aware reuse of existing prefixes; the first request may still create a valid entry, while later hits do not append new tail entries.",
      },
      strictClient: {
        name: "Client breakpoints only",
        description:
          "Accepts only explicit cache_control breakpoints and never guesses stable boundaries, for auditable cache behavior.",
      },
      sharedSession: {
        name: "Shared session cache",
        description:
          "Shares cache state by group and session with independent read/create ratios, useful when multiple accounts serve one Claude Code session.",
      },
      conservativeUsage: {
        name: "Conservative usage",
        description:
          "Caps cache read/write and output reporting with low-variance target sampling for predictable usage values.",
      },
      longContextGuard: {
        name: "Long-context guard",
        description:
          "Allows longer stable prefixes while enforcing a 96k input guard, 48k read cap, and 8k per-request creation cap.",
      },
      noCache: {
        name: "No cache",
        description:
          "Maps to the no-cache route: disables local cache reads, writes, and simulated usage so responses keep upstream usage.",
      },
    },
    ratioModes: {
      uniform: "Uniform ratio",
      independent: "Independent read/write ratios",
    },
    usageModes: {
      raw: "Report raw upstream value",
      preserve: "Preserve calculated value",
      sampleMax: "Limit to maximum",
      sampleTarget: "Sample around target",
      rawShort: "Raw",
      preserveShort: "Preserve",
      sampleMaxShort: "Max",
      sampleTargetShort: "Target",
    },
    breakpointModes: {
      clientOnly: "Client breakpoints only",
      hybrid: "Hybrid breakpoints",
      auto: "Automatic breakpoints",
    },
    scopeModes: {
      groupAccountSession: "Group + account + session",
      groupSession: "Group + session (cross-account)",
    },
    dynamicContentModes: {
      exclude: "Exclude dynamic content",
      allow: "Allow dynamic content (high risk)",
    },
    currentSelection: "{count} groups selected",
    noGroups: "No groups available for binding",
    copyName: "{name} copy",
    copySuccess: "Cache strategy duplicated",
    saveSuccess: "Cache strategy saved",
    toggleSuccess: "Cache strategy status updated",
    deleteSuccess: "Cache strategy deleted",
    unbound: "No groups bound",
    groupCount: "{count} groups",
  },
};
