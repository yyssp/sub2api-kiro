export default {
  cacheStrategies: {
    multiplier: {
      "title": "Direct usage multiplier",
      "hint": "Multiply raw upstream usage fields above their own thresholds, with no session, cache-hit or cold/hot checks. Apply the multiplier to the whole field only when its raw value strictly exceeds the threshold, then round. Model, request and final usage limits still apply.",
      "threshold": "Threshold (tokens, strictly above)",
      "factor": "Multiplier",
      "help": "1 = unchanged; 1.5 = increase 50%; 0.5 = decrease 50%. Input and meaningful output have a final minimum of 1, even with multiplier 0. Cache reads and writes may be cleared. Zero upstream output is estimated from content; truly empty replies are not billed.",
      "example": "Example: output threshold 1,000 and multiplier 1.5 leaves 1,000 unchanged and turns 1,200 into 1,800.",
      "enabled": "Enable usage multipliers",
      "input": "Uncached input",
      "output": "Output",
      "cache_read": "Cache read",
      "cache_creation": "Cache write",
      "gemini": "Gemini output includes candidates and thoughts, preserving their proportions. Generation usage has no cache-write bucket or write multiplier.",
      "preview": "Example with 1,200 tokens"
},

    audit: {
      partialSave: "Strategy settings were saved, but group bindings failed. Retry to update the same strategy.",
      bindingsLoadFailed: "Could not load group bindings. Saving is paused to preserve existing bindings. Close and reopen the editor.",
    "limits": "Final usage limits",
    "limitsHint": "0 reads the model catalog; unknown models fall back to 200k context and 64k output. Uncached input, reads and writes share the input budget. The smallest model, request and policy limits take precedence over multipliers, sampling and cost floors. Zero-usage repair and the minimum of 1 for input and meaningful output also apply to disabled, skipped and Plus raw projection.",
    "inputLimit": "Total input limit (0 = automatic)",
    "outputLimit": "Output limit (0 = automatic)",
    "defaultsHint": "Complete defaults are filled in even without a template. Advanced options are optional. The top-level preserve-upstream switch is authoritative."
},

    protocols: {
      usage_multiplierHint: "Multiply raw upstream usage fields above their own thresholds, with no session, cache-hit or cold/hot checks. Apply the multiplier to the whole field only when its raw value strictly exceeds the threshold, then round. Model, request and final usage limits still apply.",
    "uncached_input_max_tokens": "Uncached input restore upper bound",
    "reported_input_min_tokens": "Minimum total input (subject to final limits)",
    "final_cache_read_jitter_min_tokens": "Minimum read cap deduction",
    "final_cache_read_jitter_max_tokens": "Maximum read cap deduction",
    "final_output_jitter_min_tokens": "Minimum output cap deduction",
    "final_output_jitter_max_tokens": "Maximum output cap deduction",
    "output_uplift_min_tokens": "Output uplift threshold",
      prefixHint: "Match continuous systemInstruction, tools and completed contents prefixes. Cold requests have no reads; successful completions update state.",
      tool_awareHint: "Track native parts, functionCall and functionResponse boundaries for tool conversations while preserving continuous prefix matching.",
      stable_segment_cacheHint: "Track stable system, tools and history boundaries with segment thresholds, a history window, uncached input range and state write limits.",
      prefix_tool_plusHint: "Apply independent multipliers, jitter and bounds to uncached input, reads and output after tool prefix tracking; no generation write usage.",
      bounded_high_cacheHint: "Shape reads on an existing prefix, then enforce final input, read and output ceilings. Anthropic write buckets and write pricing do not apply.",
      outputGuard: "Enable final output guard",
      outputUpliftEnabled: "Enable output uplift",

      "label": "Platform",
      "all": "All platforms",
      "hint": "Policies match the platform assigned to a group. Only platforms with cache shaping support are listed. Existing policies default to Anthropic.",
      "noCreationShort": "No generation write bucket",
      "geminiHint": "Gemini: input excludes cached tokens; cache reads use cachedContentTokenCount; output includes candidates and thoughts. promptTokenCount = input + reads. Generation usage has no cache-write tokens. Explicit cache storage retains existing token-hour billing and authoritative reads.",
      "usage": "Gemini usage controls",
      "input": "Uncached input",
      "cache_read": "Cache reads",
      "output": "Output including thoughts",
      "raw": "Raw",
      "preserve": "Preserve current",
      "sample_max": "Sample maximum",
      "sample_target": "Sample target",
      "max_tokens": "Sampling maximum (0 unlimited)",
      "target_tokens": "Sampling target",
      "normal_max_multiplier": "Target upper multiplier",
      "move_delta": "Move reduced input into reads when a cache hit exists",
      "stateHint": "Prefix state writes only affect later hits and never become billable cache-write tokens. Only completed successful responses commit state. Image and file requests retain upstream cache evidence.",
      "coverage_ratio": "Stable prefix coverage ratio",
      "read_ratio": "Read multiplier",
      "min_cacheable_tokens": "Minimum cacheable prefix tokens",
      "uncached_input_min_tokens": "Uncached input floor",
      "reported_input_max_tokens": "Total input ceiling (0 unlimited)",
      "default_ttl_seconds": "Prefix TTL in seconds",
      "state_ttl_seconds": "State lifetime (seconds, 0 follows TTL)",
      "max_coverage_tokens": "Prefix coverage ceiling",
      "max_new_creation_tokens_per_request": "New prefix state tokens per request (0 unlimited)",
      "token_scale": "Total input multiplier",
      "scale_min_input_tokens": "Input scaling threshold",
      "max_simulated_input_tokens": "Simulated total input ceiling",
      "max_entries_per_scope": "Maximum entries per scope",
      "max_entries_global": "Maximum global entries",
      "estimated_bytes_limit": "Estimated state byte limit",
      "expire_after_idle_seconds": "Idle expiry in seconds",
      "scope": "Cache scope",
      "groupSession": "Group and session",
      "accountSession": "Group, account and session",
      "cache_system": "Cache system instructions",
      "cache_tools": "Cache tool definitions",
      "cache_history": "Cache completed history",
      "cache_tool_results": "Cache tool results",
      "incremental_create_enabled": "Allow incremental prefix state updates",
      "allow_derived_session": "Derive session from system, tools and first turn",
      "cold_request_guard": "Prevent synthetic cold reads and limit input uplift",
      "preserve_upstream_cache_usage": "Preserve upstream cache reads",
      "hit": "Shape read ratio on existing hits",
      "plus": "Plus: independent three-field multipliers, jitter and bounds",
      "finalRead": "Final cache read ceiling",
      "finalOutput": "Final output ceiling",
      "outputUplift": "Output uplift (%)"
},
    advanced: {
      "title": "Additional cache shaping",
      "stable_segment_cache": "Stable segment cache",
      "stable_segment_cacheHint": "Track stable tools, system and history prefixes with a history window, uncached input range, minimum segment size and incremental writes.",
      "prefix_tool_plus": "Prefix tracking Plus",
      "prefix_tool_plusHint": "Shape four fields independently with stream and small-request profiles; creation frequency uses amplified writes.",
      "bounded_high_cache": "Bounded high cache",
      "bounded_high_cacheHint": "Keep final field bounds authoritative and carry uncompensated cost across successful requests.",
      "strictStream": "Strict cache eligibility for streams",
      "strictNonStream": "Strict cache eligibility for non-streaming",
      "coldGuard": "Constrain cold usage to real input",
      "stateTTL": "State lifetime (seconds; 0 keeps legacy behavior)",
      "eligibilityHint": "Strict checks require a client cache marker and the model minimum input. Ineligible requests preserve upstream usage. State lifetime is separate from displayed 5m/1h and is capped at 300 seconds.",
      "hitTitle": "Hit ratio and cost floor",
      "enabled": "Enable hit shaping",
      "read_aware_cost_floor": "Include reads in cost-floor compensation",
      "cost_floor_top_up_input": "Top up remaining cost in input",
      "target_hit_ratio": "Target hit ratio (0–0.99)",
      "ratio_jitter": "Upward ratio jitter (0–0.2)",
      "min_cache_read_tokens": "Minimum nonzero read",
      "min_cache_creation_tokens": "Minimum nonzero creation",
      "min_cost_ratio": "Minimum cost ratio",
      "max_read_multiplier": "Maximum read / raw input (0 unlimited)",
      "min_creation_ratio": "Minimum creation / raw input",
      "boundedHint": "Priority: bounds > creation frequency > hit ratio > cost floor. Disabling input top-up discards uncompensated cost.",
      "stableTitle": "Stable segments",
      "min_total_input_tokens": "Minimum request input",
      "min_segment_tokens": "Minimum stable prefix",
      "reported_input_min_tokens": "Uncached input minimum",
      "reported_input_max_tokens": "Uncached input maximum (0 unlimited)",
      "history_message_limit": "History message limit (0 unlimited)",
      "honor_explicit_cache_control": "Honor explicit cache markers",
      "auto_cache_system": "Cache system automatically",
      "auto_cache_tools": "Cache tools automatically",
      "auto_cache_history_message_ends": "Cache history boundaries automatically",
      "plusTitle": "Plus shaping",
      "nonStreamMode": "Non-streaming shaping",
      "suppressedTarget": "Suppressed creation target",
      "smallEnabled": "Override small requests",
      "smallMax": "Maximum small-request input",
      "smallMode": "Small-request shaping",
      "disableCache": "Disable cache for small requests",
      "inherit": "Inherit",
      "custom": "Custom",
      "raw": "Raw split",
      "input": "Uncached input",
      "output": "Output",
      "cache_read": "Cache read",
      "cache_creation": "Cache creation",
      "drop": "Drop",
      "defaultProfile": "Default profile",
      "nonStreamProfile": "Non-streaming profile",
      "smallProfile": "Small-request profile",
      "multiplier": "Multiplier",
      "jitter_ratio": "± jitter ratio",
      "min_tokens": "Nonzero minimum",
      "max_tokens": "Maximum (0 unlimited)",
      "max_jitter_tokens": "Maximum cap deduction",
      "overflowToInput": "Move cache overflow to input"
},
    title: "Cache Strategies",
    description:
      "Create reusable cache shaping policies for platforms such as Anthropic and Gemini, then bind them to groups.",
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
    CACHE_STRATEGY_BOUND_TO_GROUPS:
      "This cache strategy is still bound to {bound_group_count} group(s). Unbind the groups before deleting it.",
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
      outputUpliftEnabled: "Enable output uplift",
      outputUpliftEnabledHint:
        "When off, neither the threshold nor the percent applies. Values are kept, so re-enabling needs no retyping.",
      outputUpliftMinTokens: "Output uplift threshold",
      outputUpliftPercent: "Output uplift percent",
      finalOutputGuardEnabled: "Enable output guard (uplift and cap)",
      finalOutputGuardEnabledHint:
        "Turning this off disables both the output uplift and the output cap without clearing their values. Enabled by default when unset.",
      finalOutputMaxTokens: "Output cap (0=off)",
      finalOutputJitterMinTokens: "Cap jitter range min",
      finalOutputJitterMaxTokens: "Cap jitter range max",
      finalCacheReadMaxTokens: "Cache-read cap (0=off)",
      finalCacheReadJitterMinTokens: "Cap jitter range min",
      finalCacheReadJitterMaxTokens: "Cap jitter range max",
      finalCacheCreationMaxTokens: "Cache-write cap (0=off)",
      finalCacheCreationJitterMinTokens: "Cap jitter range min",
      finalCacheCreationJitterMaxTokens: "Cap jitter range max",
      finalCapJitterHint:
        "When a value hits the cap it is pulled back by a deterministic amount in this range, so capped records do not all report the same number; retries of the same request stay stable. Keep the lower bound well below the cap and ensure the deduction cannot reduce the result to 0. Small caps scale the range proportionally; when the cap is 0 (off) the range is cleared automatically.",
      finalCapJitterValidationHint:
        "Recommended: keep the lower bound meaningfully below the cap and the upper bound below the cap. Otherwise normalization scales the range and narrows the variation at the cap.",
      finalCapJitterValidationError:
        "Invalid cap jitter range: the upper bound must be greater than the lower bound, and the lower bound must be below the final cap. Disable jitter when the final cap is 1.",
      skipNonStreamUsageProjection: "Skip usage projection for non-streaming",
      skipNonStreamUsageProjectionHint:
        "Non-streaming responses carry complete upstream usage. Enable this to pass it through untouched instead of applying cache shaping. Streaming responses are unaffected.",
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
        'Cache namespaces are isolated by group, session, strategy revision, and protocol by default (scope "group + session"), so switching accounts within a session still hits the existing prefix. Choosing "group + account + session" adds per-account isolation, which starts a new cache on every account switch.',
      coverageRatio: "Cache-evidence coverage ratio",
      coverageRatioHint:
        "The share of the stable prefix eligible for caching; lowering it reduces both later reads and writes.",
      usageRatio: "Cache-evidence total ratio",
      usageRatioHint:
        "Scales reported cache-read/create evidence; it mainly changes usage numbers, not tracker capacity.",
      ratioMode: "Cache-evidence ratio mode",
      ratioModeHint:
        "Uniform applies one ratio to read and creation; independent lets you control them separately.",
      readRatio: "Read-evidence ratio",
      readRatioHint:
        "Only used in independent mode; lowering it reduces reported cache_read without deleting cached prefixes.",
      creationRatio: "Creation-evidence ratio",
      creationRatioHint:
        "Only used in independent mode; lowering it reduces reported cache_creation, not the hard write limit.",
      breakpointMode: "Breakpoint mode",
      breakpointModeHint:
        "Chooses client, automatic, or hybrid breakpoints; without a usable breakpoint there is no cache read/write.",
      minCacheableTokens: "Minimum cacheable tokens",
      minCacheableTokensHint:
        "⚠️ Setting this too high disables the whole strategy. It compares the cumulative prefix tokens at each " +
        "breakpoint, not the total request size; when every breakpoint falls below it the entire cache profile is " +
        "discarded and the request reports no cache_read / cache_creation at all. " +
        "0 (the default) removes the gate entirely so every breakpoint is cacheable. " +
        "It only filters; small requests are never padded up to this value.",
      maxCoverageTokens: "Maximum coverage tokens",
      maxCoverageTokensHint:
        "Absolute cap on the cached prefix; reads can continue after it is reached while new creation tapers to zero.",
      maxCreationTokens: "Maximum creation tokens per request",
      maxCreationTokensHint:
        "Hard cap on the real prefix added by one request; distinct from the final usage write cap.",
      defaultTtlSeconds: "Default TTL (seconds)",
      defaultTtlSecondsHint:
        "Lifetime of ordinary cache breakpoints; after expiry the next request must create them again.",
      hourTtlSeconds: "1-hour TTL (seconds)",
      hourTtlSecondsHint:
        "Lifetime for breakpoints marked as one-hour; it must be at least the default TTL and remains system-bounded.",
      forcedTtlTier: "Forced reporting tier",
      forcedTtlTierHint:
        "The reported tier resolves through four levels: forced tier, upstream response, client declaration, then 5m. This field is the highest. Choose Follow to leave it unforced, or pick 5m/1h to report that tier regardless of upstream and client. It only shapes the billing split reported to clients; upstream cost is unchanged.",
      forcedTtlTierFollow: "Follow (not forced)",
      forcedTtlTier5m: "Force 5 minutes",
      forcedTtlTier1h: "Force 1 hour",
      trustUpstreamTtlTier: "Trust upstream tier",
      trustUpstreamTtlTierHint:
        "When enabled, the tier the upstream actually returned is the one reported, keeping revenue aligned with cost. If a third-party upstream ignores the client declaration (client asked for 5m but upstream bills 1h), disabling this leaves the platform absorbing the difference. Falls back to the client declaration when upstream reports no tier.",
      tokenScale: "Token scale",
      tokenScaleHint:
        "Scales long-input usage simulation after the threshold; it does not change the prompt sent upstream.",
      scaleMinInputTokens: "Minimum input tokens for scaling",
      scaleMinInputTokensHint:
        "Token scaling starts only after this input size; short requests are left at their normal scale.",
      maxSimulatedInputTokens: "Maximum simulated input tokens",
      maxSimulatedInputTokensHint:
        "Total usage-simulation ceiling; large-window templates need enough headroom for read and creation together.",
      capJitterMinTokens: "Input cap jitter min",
      capJitterMinTokensHint:
        "Minimum deterministic deduction when the simulated input cap is actually reached.",
      capJitterMaxTokens: "Input cap jitter max",
      capJitterMaxTokensHint:
        "Maximum deterministic deduction at the simulated input cap; keep it below the cap.",
      maxEntriesPerScope: "Maximum entries per scope",
      maxEntriesPerScopeHint:
        "Maximum entries for one group/session scope; older entries are evicted after the limit.",
      maxEntriesGlobal: "Maximum global entries",
      maxEntriesGlobalHint:
        "Total entries across all scopes; old entries are evicted when the global limit is exceeded.",
      estimatedBytesLimit: "Estimated cache byte limit",
      estimatedBytesLimitHint:
        "Approximate memory ceiling for the tracker; reaching it evicts old entries without changing usage math.",
      expireAfterIdleSeconds: "Idle expiration (seconds)",
      expireAfterIdleSecondsHint:
        "Deletes entries that have been idle for this long; dormant sessions usually need a new creation on return.",
      currentUserStablePrefix: "Cache current-user stable prefix",
      currentUserStablePrefixHint:
        "Includes a stable prefix of the current user message; it can raise large-window writes/next-turn reads but is less conservative.",
      currentUserStablePrefixMaxTokens: "Maximum user-prefix tokens",
      currentUserStablePrefixMaxTokensHint:
        "Maximum tokens taken from the current user stable prefix; effective only when the toggle is enabled.",
      scopeMode: "Cache scope",
      scopeModeHint:
        "Group + session allows cross-account reuse; adding account isolation recreates the cache after account switches.",
      dynamicContentMode: "Dynamic content handling",
      dynamicContentModeHint:
        "Excluding dynamic fields is usually more stable; allowing them can lower hit rate and repeat creation.",
      allowDerivedSession: "Allow derived sessions",
      allowDerivedSessionHint:
        "Creates a derived scope when the request has no session id; this covers more traffic but weakens boundaries.",
      preserveUpstreamCacheUsage: "Prefer upstream cache usage",
      incrementalCreation: "Enable incremental creation",
      incrementalCreationHint:
        "Allows new stable prefixes after a hit; disabling it stops additions without deleting existing reads.",
      cacheSystem: "Cache system",
      cacheSystemHint:
        "System content enters the stable prefix; larger system prompts usually raise the base size of later cache reads and writes.",
      cacheTools: "Cache tools",
      cacheToolsHint:
        "Tool definitions enter the stable prefix; stable definitions improve hits, while frequent changes reduce prefix stability.",
      cacheHistory: "Cache message history",
      cacheHistoryHint:
        "Message history enters the stable prefix; in long sessions it is often the main source of cache_read tokens.",
      cacheToolResults: "Cache tool results",
      cacheToolResultsHint:
        "Tool results enter the stable prefix; dynamic IDs, timestamps, and similar fields can cause repeated creation.",
      creationControlEnabled: "Enable creation throttling",
      creationControlEnabledHint:
        "Controls creation frequency, per-event release, and window budget; it does not limit existing cache reads.",
      minCreationDeltaTokens: "Minimum creation delta tokens",
      minCreationDeltaTokensHint:
        "Attempts below this delta wait and can accumulate for a later release; larger values usually mean fewer writes.",
      minSuccessfulRequestsBetween: "Successful requests between creations",
      minSuccessfulRequestsBetweenHint:
        "Successful requests required between creation releases; intervening requests may still read cache.",
      minCreationIntervalSeconds: "Minimum creation interval (seconds)",
      minCreationIntervalSecondsHint:
        "Shortest time between creation releases; rapid requests are more likely to be suppressed.",
      maxCreationTokensPerEvent: "Maximum creation tokens per event",
      maxCreationTokensPerEventHint:
        "Maximum creation value released to usage for one event; use the per-request limit as well to cap real additions.",
      creationBudgetWindowSeconds: "Creation budget window (seconds)",
      creationBudgetWindowSecondsHint:
        "Length of the window used to count creation budget; the budget is recalculated after the window ends.",
      maxCreationTokensPerWindow: "Maximum creation tokens per window",
      maxCreationTokensPerWindowHint:
        "Total creation allowed to be released in one window; once exhausted, creation may be zero while reads remain.",
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
        name: "Blank custom strategy",
        description:
          "Start from the defaults and configure cache scope, read/write ratios, usage shaping, and creation pacing yourself.",
      },
      kiroRsTool: {
        name: "kiro-rs-tool",
        description:
          "Full coverage with no token scaling and no creation throttling, so reported usage tracks real prefix hits. Uncached input stays within 32-4096. Suited to tool-heavy Claude Code sessions.",
      },
      highCache: {
        name: "High cache (default)",
        description:
          "A general Claude Code-compatible high-cache baseline: stable prefixes, 98% evidence ratios, long-input scaling, and final usage caps. The 100k per-event creation cap fits normal production traffic.",
      },
      steadyGrowth: {
        name: "Steady growth",
        description:
          "Writes up to a 100k per-event cap so the cache climbs steadily. Minimum interval, request count, and window budget limits are disabled for growth-oriented observation.",
      },
      rapidGrowth: {
        name: "Rapid growth",
        description:
          "Reaches a large value within a few turns with irregular increments. A 120k per-event cap and 2M window budget fit high-throughput stress runs.",
      },
      largeWindow: {
        name: "Large read/write (700k read / 500k write)",
        description:
          "Allows both buckets to reach large values: a 700k final cache-read cap and a 500k final cache-creation cap. Raw creation and relaxed pacing expose near-cap values without forcing every turn to the same number.",
      },
      largeReadControlledWrite: {
        name: "Large read, controlled write (700k read / 120k target write)",
        description:
          "Keeps 700k cache-read capacity while shaping creation around a 120k target with a 180k cap and paced release. Use when high hit volume matters but every turn should not write a large usage bucket.",
      },
      largeReadSmallWrite: {
        name: "Large read, small write (700k read / ~30k write)",
        description:
          "Allows cache reads up to 700k while targeting roughly 30k creation with a 60k cap and small budget. Use when cache hits are valuable but write cost or write frequency must stay low.",
      },
      largeWriteControlledRead: {
        name: "Large write, controlled read (500k write / 250k read)",
        description:
          "Keeps 500k cache-creation capacity while limiting final cache-read reporting to 250k. The read limit only shapes downstream usage; it does not delete cached prefixes from the tracker.",
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
