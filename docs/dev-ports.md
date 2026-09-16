# 本地开发端口

本地开发（热更新）用的是高位端口，**不是**根 `README.md` 里的 `8080`。
根 README 描述的是打包部署形态（`SERVER_PORT=8080`、docker-compose 端口映射、
setup 向导地址），那套端口在本地开发环境下不成立。

| 服务 | 端口 | 进程 | 启动方式 |
| --- | --- | --- | --- |
| 前端 | `48780` | vite dev server（node） | `cd frontend && pnpm dev` |
| 后端 | `48788` | `backend/tmp/server` | `cd backend && air`（见 `.air.toml`） |

前端在 `vite.config.ts` 里把 `/api`、`/v1`、`/setup` 反代到后端。

## 确认服务在跑

```bash
lsof -nP -iTCP -sTCP:LISTEN | grep 4878
curl -sS http://127.0.0.1:48788/health     # {"status":"ok"}
```

## 联调 / 抓包直连后端

调 API、压测、抓包一律直连 `48788`，绕开 vite 代理那一层 ——
代理会 `changeOrigin`，多一层改写只会干扰观测。

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:48788
```

历史记录里出现过的 `28080/26432/26380` 属于已结束的隔离黑盒阶段，
当前口径以 `48780/48788` 为准（另见
`openspec/changes/add-group-cache-strategy/README.md`）。
