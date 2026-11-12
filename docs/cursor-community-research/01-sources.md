# 01 调研范围与来源

「活跃」指最后一次提交在 2026-07-01 或之后，并且仓库没有归档。日期都是 2026 年，格式为 MM-DD。

实现方式的缩写：
- **Run**：`agent.v1.AgentService/Run`，HTTP/2 双向流。
- **SSE+Bidi**：`RunSSE` 加 `aiserver.v1.BidiService/BidiAppend`，走 HTTP/1.1。
- **Sand**：`aiserver.v1.InferenceService/Stream`。
- **旧 Chat**：已经退役的 `ChatService/StreamUnifiedChatWithTools`。
- **SDK**：官方 `@cursor/sdk` 加 `crsr_` key。
- **CLI**：包一层 `cursor-agent` 命令行。

## 覆盖统计

**A 协议/代码检索**
- 用 `gh api search/code` 跑了三类查询：12 个单词条、22 个「上游标识 + 下游路由」组合、24 个按语言拆分的查询。合计 2667 个唯一仓库，其中活跃 2114 个。
- 同时命中上游和下游标识的有 549 个，其中含新协议标识的 316 个，只含旧协议标识的 62 个。
- 另做了关键词仓库检索，得到 1291 个。与代码检索取并集后共 3908 个，其中活跃 3042 个。
- 分诊后完整克隆 106 个，5 个大网关只稀疏检出 Cursor 目录。
- 对 34 个基底项目逐页枚举了全部 fork。

**B 网关与 fork**
- 对 9 个主流网关做了 fork 全量枚举，过滤条件是「7 月后有推送，且推送晚于创建」，然后对 ahead>0 的 fork 扫描目录树。
- 命中 Cursor 实现的 fork 数量：
  - router-for-me/CLIProxyAPI：28 个（基底 8949 个 fork）
  - Wei-Shaw/sub2api：9 个（基底 9924 个 fork）
  - QuantumNous/new-api：2 个
  - nianzs/sub2api：1 个
  - litellm、gpt4free、claude-relay-service、AIClient2API、claude-code-router：0 个

**C GitHub 之外**
- GitHub issues/PR/commit 检索：22 次查询，1862 条命中，涉及 1081 个仓库，分诊后新增 17 个相关仓库。
- npm：60 个 Cursor 相关包，12 个相关。
- Docker Hub：71 个镜像，7 月后更新的 9 个，逐层检查了 4 个。
- 社区：linux.do 7 帖、V2EX 3 帖、NodeSeek 2 帖、Reddit/HN/博客 8 篇。
- 以下来源没有相关结果：Gitee（API 搜索全部返回空）、GitLab（只找到 1 个过时镜像）、Codeberg、PyPI、gist。

**局限**
- GitHub 代码检索只索引默认分支，且只收录小于 384KB 的文件，深度 vendored 或 minified 的实现可能漏掉。
- 测试文件只计了数量，没有运行。

## A 协议/代码检索：已核验的实现

以下都人工确认过是真正的帧解析，即 5 字节 Connect 头加 protobuf。

| 仓库 | 最后提交 | 上游 | 下游 | 结论 |
|---|---|---|---|---|
| [YeautyYE/claude-cursor-proxy](https://github.com/YeautyYE/claude-cursor-proxy) | 09-09 | Run + SSE/HTTP1 兜底 + Sand | Messages、count_tokens、Responses | active。Cursor 部分代码量最大，README 的 Limitations 表是一份现成的线上故障清单 |
| [greenSheep999/cursor-proto](https://github.com/greenSheep999/cursor-proto) | 09-12 | SSE+Bidi、Sand、旧 Chat、SDK | OpenAI、Messages、Responses、Gemini | active。协议还原最完整，`docs/phase-*` 记录了大量实测 |
| [mediavee/cpa-cursor](https://github.com/mediavee/cpa-cursor) | 11-10 | Run（CLI 身份 `cli-2026.10.22`） | 经 CLIProxyAPI | active。工具轮可以挂起后续接，错误分类完整 |
| [lidge-jun/opencodex](https://github.com/lidge-jun/opencodex)（`src/adapters/cursor`） | 11-12 | Run + SSE+Bidi | Codex Responses、Messages、chat | active。checkpoint 续接、图片治理、HTTP/1 模拟双向流 |
| [doitcan-oiu/cursor-proxy](https://github.com/doitcan-oiu/cursor-proxy) | 11-07 | Run + 旧 Chat | chat、Messages + 管理后台 | active。产品形态和 sub2api 最接近 |
| [raine/claude-code-proxy](https://github.com/raine/claude-code-proxy) | 11-06 | Run（HTTP/2） | Messages | active，705★。YeautyYE 是从它 fork 出去的 |
| [KonghaYao/cursor2api](https://github.com/KonghaYao/cursor2api) | 09-28 | `crsr_` 换 JWT 后走 Run | chat、Messages、Responses | active。结构化多轮（root blob），`CLAUDE.md` 记录了踩坑过程 |
| [diegosouzapw/OmniRoute](https://github.com/diegosouzapw/OmniRoute) | 11-11 | Run + 旧 Chat | 整个网关 | active，86k★ 的网关 |
| [taylorzhangyx/cursor-gateway](https://github.com/taylorzhangyx/cursor-gateway) | 11-11 | Run + RunSSE | chat、Responses | active，与 hnewcity/cursor-gateway 同源 |
| [enderzcx/cursor-agent2api](https://github.com/enderzcx/cursor-agent2api) | 09-19 | agent.v1 + Sand | 经 CPA | active |
| [timmyagentic/cliproxyapi-cursor-plugin-next](https://github.com/timmyagentic/cliproxyapi-cursor-plugin-next) | 10-27 | SSE+Bidi | 经 CPA | active |
| [D3Dream/cursor2api](https://github.com/D3Dream/cursor2api) | 10-12 | Run | chat、Messages、Responses | active，与 Owen0225/cursorAgent2Api 同源 |
| [BaiMeou/cursor-sand2api](https://github.com/BaiMeou/cursor-sand2api) | 09-03 | Sand + `crsr_` 走 Run | chat、Responses | active |
| [AstroQore/cursor2api](https://github.com/AstroQore/cursor2api) | 09-04 | Run（HTTP/2） | chat | active。fork jddj007-hydra 10-15 仍在更新 |
| [7iook/cursor-bridge](https://github.com/7iook/cursor-bridge) | 08-13 | Run（与 cursor-agent 字节级一致） | Messages | active。`bridge/README.md` 记录了大量实测 |
| [decolua/9router](https://github.com/decolua/9router) | 11-11 | Run + 旧 Chat | 网关 | active，32k★ |
| [akapug/cursor-openai-api](https://github.com/akapug/cursor-openai-api) | 10-21 | Run bridge | chat | active，egoist 的 fork |
| [tonycoder-hub/cursor-api-proxy](https://github.com/tonycoder-hub/cursor-api-proxy) | 10-14 | SSE+Bidi，只做文本 | chat | active |
| [jacobsparts/cursor-gateway](https://github.com/jacobsparts/cursor-gateway) | 09-17 | SSE+Bidi | chat | active |

同样带有真 Cursor executor 的网关还有：kaitranntt/CLIProxyAPIPlus（11-05）、AprilNEA/BYOKEY、aio-proxy/aio-proxy、risunCodex/Cartethyia、576469377/Agent2API。

**插件形态**：不对外暴露 HTTP API，但协议实现扎实，可以参考：
- [nkoynov/cursor-opencode-provider](https://github.com/nkoynov/cursor-opencode-provider)（11-11）
- [Rahularya01/pi-cursor](https://github.com/Rahularya01/pi-cursor)：它的 fork 很活跃，例如 wg138940 做了历史改写后轮换会话，Laurens-Nys 做了拒绝未知 exec 以免卡住。
- [can1357/oh-my-pi](https://github.com/can1357/oh-my-pi)
- [papodaca/cursor-rpc](https://github.com/papodaca/cursor-rpc)：协议规格文档很完整。

**走官方 SDK、不逆向的**（作为备选形态）：Sunnyender-org/cursor-sdk2api（fork aosika3324、XiaoXiaoXian 等）、NGLSG/Cursor2API、xjoker/cursor-api、iFwu/cursor2response、dwgx/cursorapi。

**CLI 包装**：anyrobert/cursor-api-proxy（284★）、Nomadcxx/opencode-cursor（723★）、ITboys/cursor-agent-api-proxy、M507/curapi。

## B 网关与 fork

| 仓库 | 最后提交 | 实现方式 | 结论 |
|---|---|---|---|
| [trzjy/sub2api](https://github.com/trzjy/sub2api) | 11-11 | Run 全双工 + checksum；token 从第三方卡密服务拉取 | active。比上游多 1432 次提交，偏向倒卖，不建议参考 |
| [sv3nbeast/sub2api](https://github.com/sv3nbeast/sub2api) | 10-04 | Run（agentn.api5）+ deep-login OAuth + `exchange_user_api_key` | active |
| [kubelandy/sub2api](https://github.com/kubelandy/sub2api) | 10-19 | Run，保留旧 Chat 常量 | active，与 maodapeng1 同一提交系列 |
| [maodapeng1/sub2api](https://github.com/maodapeng1/sub2api) | 09-28 | Run + deep-control OAuth，工具交给调用方执行 | active，nianzs 的 fork |
| [kaitranntt/CLIProxyAPIPlus](https://github.com/kaitranntt/CLIProxyAPIPlus) | 11-05 | Run/GetUsableModels（HTTP/2）+ loginDeepControl | active |
| [endverse/cpa-cursor-plugin](https://github.com/endverse/cpa-cursor-plugin) | 10-27 | Run + SSE+Bidi，原生支持 Messages/Responses | active（CPA 官方商店插件） |
| [yobo2u/omsub](https://github.com/yobo2u/omsub) | 10-01 | Run/GetUsableModels + `exchange_user_api_key`；源码在 `cursor` 分支 | active |
| [spancerxing/cliproxyapi-cursor-plugin](https://github.com/spancerxing/cliproxyapi-cursor-plugin) | 11-04 | SSE+Bidi，浏览器登录或 API key 登录 | active |
| [edgebyte-ai/cliproxyapi-cursor-provider-plugin](https://github.com/edgebyte-ai/cliproxyapi-cursor-provider-plugin) | 10-31 | Run，多账号 | active |
| [davidfarah2003/cliproxy-cursor-provider](https://github.com/davidfarah2003/cliproxy-cursor-provider) | 08-22 | Run + checksum | active |
| [gusnips/cc-proxy](https://github.com/gusnips/cc-proxy) | 11-11 | Run（Rust），protobuf 类从 cursor-agent 的 bundle 中加载 | active |
| [yxsyzht-pixel/new-api](https://github.com/yxsyzht-pixel/new-api) | 11-11 | new-api 渠道，转发到基于 CLI 的 Cursor-Plan2API 桥 | active |
| npm [@wengine-ai/ccr-provider-cursor](https://www.npmjs.com/package/@wengine-ai/ccr-provider-cursor) | 11-04 | Run + HTTP/1.1 回退；支持 PKCE、导入 cursor-agent 凭证、换 API key | active，仓库私有 |
| UNICKCHENG/CLIProxyAPIPlus、oakimov/claude-code-router、michael-her/litellm | 10 月 | 官方 SDK 桥 | 非逆向路线 |

以下项目确认没有 Cursor provider：CLIProxyAPI 主仓、new-api、one-api、AIClient2API、Wei-Shaw/sub2api、nianzs/sub2api、claude-relay-service、claude-code-router、litellm、gpt4free、bifrost、llmgateway、claude-code-hub、cc-switch、Antigravity-Manager、opencode 等。

## C GitHub 之外的来源

| 来源 | 最后活动 | 实现方式 | 结论 |
|---|---|---|---|
| [tifuck/fuse](https://github.com/tifuck/fuse) | 11-11 | SSE+Bidi，cursor-agent PKCE 登录，共享订阅池 | NEW_ACTIVE |
| [rushteam/aiopt](https://github.com/rushteam/aiopt) | 11-11 | Run（node:http2），还没有 HTTP/1.1 回退 | NEW_ACTIVE |
| [Anon-deisu/CLIProxyAPI-workbuddy](https://github.com/Anon-deisu/CLIProxyAPI-workbuddy) | 11-08 | Run（HTTP/2）+ `exchange_user_api_key` + checksum | NEW_ACTIVE |
| [PsychedelicShayna/NeoProxy PR #57](https://github.com/PsychedelicShayna/NeoProxy/pull/57) | 11-06 | Run + HTTP/1.1 回退（移植自 NeoPi） | 只在分支上活跃 |
| [veildawn/ai-provider-plugins](https://github.com/veildawn/ai-provider-plugins) | 11-11 | 签名的 Wasm 插件，把 chat/completions 改写为 Run | 网关本体闭源 |
| [logibricks-io/open-heimdall-hofund](https://github.com/logibricks-io/open-heimdall-hofund) | 11-11 | Run + SSE+Bidi + 心跳 | WIP。`docs/cursor-upstream-design.md` 是多条协议事实的出处 |
| [raufimusaddiq/routeweft](https://github.com/raufimusaddiq/routeweft) | 11-02 | Run（移植自 9router） | NEW_ACTIVE |
| [leeguooooo/agent-cli-to-api](https://github.com/leeguooooo/agent-cli-to-api) | 10-07 | CLI 包装 | NEW_ACTIVE |
| [Menci/Floway PR #154](https://github.com/Menci/Floway/pull/154) | 08-08 | SSE+Bidi（api2）；用 Durable Object 持有 socket，供工具结果续接 | 未合并的 PR |
| [QuantumNous/new-api PR #6869](https://github.com/QuantumNous/new-api/pull/6869) | 08-15 | 官方 SDK sidecar | 被拒的 PR |
| Docker Hub [imryang/cursor2api](https://hub.docker.com/r/imryang/cursor2api) | 10-20 | 基于 oh-my-pi 的 streamCursor；API key 走 `exchange_user_api_key` | 源码仓库已 404 |
| Docker Hub [michaelxxx/cursor2api](https://hub.docker.com/r/michaelxxx/cursor2api) | 09-08 | SSE+Bidi（Go），带账号池、Postgres、管理台 | 闭源 |
| [OXI-717/bb-plugins](https://github.com/OXI-717/bb-plugins) | 11-11 | 原生 Cursor RPC 透传加 ACP 包装 | 不做协议转换 |
| [yetone/magpie](https://github.com/yetone/magpie) | 11-11 | 内置 Cursor 已退役，改由 magpie-community/plugins 提供 | 通过插件可用 |

**已移除 Cursor 支持**：这些代码仍有考古价值，dimC 的多条事实出自它们。
- [knoxwh/ccextra](https://github.com/knoxwh/ccextra)：09-29 至 10-15 期间移除。
- [Alan-6902/proxy-rs](https://github.com/Alan-6902/proxy-rs)：11-05 移除。
- [delinoio/oss tairouter](https://github.com/delinoio/oss/pull/4549)：10-30 加入后当天移除。

## 已过时或排除

- **网页免费接口，不是订阅**：7836246/cursor2api（10-12 归档）、Xchat1/cursor2api-go、xuweizhengo/cursor-free-api，以及 GitLab 镜像 akcn99/cursor2api。
- **已过期**：wisdgod/cursor-api（04-01 归档，Docker 拉取 9.3k 次）、zhx47/cursor-api、JiuZ-Chn/Cursor-To-OpenAI、everestmz/cursor-rpc（2024-12），以及 Docker Hub 上 04-05 之前的 cursor2api 系镜像。
- **只实现旧 ChatService**：F0Rextasy/cursor2api、weikunzl/Cursor-gateway、darwnlinz1/CursorRouter、mydisha/keirouter、misaalanshori/LLMRoller、pwnapplehat/cursor-proxy-patched。szxypi/cpa-cursor 新旧协议混用。
- **方向相反**（BYOK，或者把 Cursor IDE 当客户端来服务）：funny-vibes/agent-vibes、soulvon/AnyBridge、lguilhermee/keybridge、AsukaCC/cursor-free、velum-labs/cursorkit、singhutsav5502/Glider、aohun/router-switch、CometixSpace/CCursor 等。
- **可疑**：sdsdsdsdsdsihdkjsdjl/cursoride2api 仓库里附带了不透明的 zip（selector.exe、lua51.dll）。
