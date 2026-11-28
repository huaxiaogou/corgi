# Corgi 工程与代码规范

本文件是仓库的架构与代码红线，所有开发者与 AI Agent 在本仓库工作时必须遵守。

## 1. 架构定位与单向依赖

Corgi 采用 **「进程内原生 API 大脑（Hub 控制面）+ 多机 PTY Workers（Node/Hub 执行面）」** 架构：大脑只负责理解意图、账本编排、风控拍板与多通道投递；所有仓库读写、命令执行与代码修改一律下沉给 PTY Workers。

严禁破坏以下单向依赖关系，严禁出现循环引用：

```text
cmd/corgi
  ├── internal/cli
  └── internal/channel/web          (HTTP/SSE/PWA 接入层与静态资源，含 fleet 升级编排)
        └── internal/channel        (Bus：多通道投递、企微流式与焦点路由)
              ├── internal/channel/wecom  (企微 WSS 协议、加解密与卡片拼装)
              └── internal/core           (仅通过 ChannelSink 接口回调，严禁 import channel/*)
                    ├── internal/fleet    (多机 yamux 隧道、配对鉴权；仅 auth 依赖 store)
                    ├── internal/gitwt    (git worktree 隔离与只读核验)
                    ├── internal/runtime/{herdr,agent,codex}
                    └── internal/store
                          └── internal/model   (纯领域模型，零内部依赖)
```

> **边界守卫**：新增非 HTTP 业务编排（例如机群升级状态机、独立运维任务）优先下沉至 `internal/core` 或专用领域包，避免继续向 `internal/channel/web` 堆叠重业务逻辑。

## 2. 后端 Go 规范（Go 1.25+）

- **依赖克制**：保持零 CGO、单二进制极简依赖（当前 `go.mod` 仅 4 个直接依赖），**禁止引入重型 Agent 框架或非必要第三方 LLM SDK**。
- **文件粒度与存量棘轮**：
  - 新文件目标 **≤ 400 行**（核心状态机不超过 600 行），按单一职责拆分。
  - **存量巨文件棘轮**：修改已显著超标的文件（如 `core/workers.go`、`core/worker_gc.go`、`core/engine.go`、`core/loop.go`、`channel/bus.go`、`web/handlers_updates.go`、`web/handlers_super.go`）时，禁止继续塞入新职责——先按子职责拆出独立文件再落地变更。
- **零可变全局状态**：
  - 禁止包级可变全局变量与包级可变测试 hook（缓存、连接、配置、Adapter 会话认领表如 `AgyAdapter` 的 `claimed` 一律挂在结构体实例上），保证 `t.Parallel()` 并发隔离与优雅停机。
  - 仅允许只读包级变量（编译期正则 `regexp.MustCompile`、哨兵错误 `errors.New`、只读常量映射）。
- **并发与锁纪律**：
  - 所有外部 IO（`herdr` Unix Socket、yamux、企微 WSS、上游 LLM HTTPS/SSE、SQLite、子进程）必须透传 `context.Context` 并显式设置超时。
  - **锁内零 IO**：`sync.Mutex` 只保护纯内存字段；读取快照后立刻解锁，再做网络/文件/子进程调用。
  - 操作受管 Worker 状态或窗格生命周期时须遵守 `beginWorkerOp` / `endWorkerOp` 互斥协议，防止与后台无损休眠 GC 竞态。
- **原生 API 大脑红线**：
  - **工具集封顶**：大脑工具严格限定在控制面操作（调度、查询、账本、提醒、记忆、配置、回复/静默），**严禁向原生大脑添加本地读盘、Shell、MCP 或编码执行工具**。
  - **协议层边界**：`internal/core/llm_wire.go` 仅维持标准 OpenAI Chat Completions 兼容层（含 SSE 60s 分片看门狗与 `reasoning_content` 透传），不在仓库内堆叠多套厂商私有 Wire 协议。
  - **事务防重（Strategy 2B）**：所有有副作用的大脑工具写操作前必须写入 `api_router_inflight` 防重标记，并在提交后执行 `AckTriggerTarget`，确保进程重启不重放。
  - **确定性权限闸门**：严禁用自然语言正则推断人类授权。后台非人类回合硬限只读派发（`mode="readonly"`）；`allow_release=true` 及 `prototype/readonly → implement` 升级强制经 `hasHumanDecisionCardApproval` 核验同线程真人决策卡点选；`manage_config` 仅限主线程且高危变更强制走 L2 确认卡 + 乐观哈希校验。
- **错误处理、日志与凭据安全**：
  - 统一用 `fmt.Errorf("<动作上下文>: %w", err)` 包装错误，禁止静默吞错。
  - 统一使用标准库 `log/slog` 结构化日志。
  - **凭据零泄露**：严禁在日志、错误响应、URL 或注入模型的上下文中泄露任何明文 Master Token、Node Token、Router API Key、Hook Key 或企微 Secret。Router API Key 必须独立存储、绑定 `BoundOrigin`，出站 LLM 强制 HTTPS + 防 SSRF 拨号 + 禁用 3xx 重定向。
- **数据库、文件系统与通道安全**：
  - SQLite WAL 走显式版本迁移（`PRAGMA user_version`），一律使用单行增量参数化 SQL，禁止整表全量覆写。
  - 持久记忆（`memory/`、`dreams/`）与上传媒体目录操作强制使用 Go 1.25 `os.Root` 锚定根目录，拒绝对符号链接读写并严格限制文件大小；HTML 预览强制 CSP 沙箱。
  - 企微通道接入层严格执行 `allow_user_ids` 发送者白名单校验，非白名单一律静默丢弃；主对话与 Worker 直聊通道严格隔离。
- **后端验收门禁**：每次后端变更提交前必须全部通过：
  ```bash
  gofmt -l .
  go vet ./...
  go test -race ./...
  ```

## 3. 前端规范（Vite + React 19 + TypeScript + Tailwind CSS v4）

- **视觉铁律（极冷峻黑白纸面风）**：
  - 所有颜色唯一定义在 `web/src/styles/tokens.css` 语义变量：
    - 基础纸面与墨色：`--bg` / `--bg-2` / `--bg-3` / `--line` / `--line-2` / `--fg` / `--fg-2` / `--fg-3` / `--accent` / `--on-accent` / `--user-bg` / `--user-fg`
    - 状态语义色：`--attn` / `--attn-soft` / `--attn-line` / `--ok` / `--danger` / `--err`
    - 富文本派生色：`--diff-*` / `--co-*` / `--mark-bg`
  - **禁止在组件中硬编码 hex 颜色、禁止高饱和 Tailwind 彩色类（如 `bg-blue-*`、`bg-purple-*`、`bg-gradient-*`）**；明暗主题切换通过 `:root[data-theme]` 与 `prefers-color-scheme` 统一驱动。
  - Tailwind 仅用于布局、间距、字号、圆角和响应式；全站 99% 黑白暖灰 + 克制琥珀/语义提醒点。
- **TypeScript 严格模式与组件边界**：
  - `tsconfig.json` 保持 `"strict": true`、`noUnusedLocals`、`noUnusedParameters`，禁止 `any`。
  - 前后端契约集中在 `web/src/types/api.ts`。
  - 按 `components/`（纯 UI 原语）、`features/`（业务模块）、`hooks/`（SSE/状态）、`lib/`（纯逻辑与 API 客户端）拆分，单组件目标 **≤ 200 行**。
  - **存量巨组件棘轮**：修改已严重超标的组件（如 `CorgiChatView.tsx`、`WorkNotebook.tsx`、`SettingsTab.tsx`、`AgentPaneView.tsx`、`ComposerBar.tsx`）时，新 UI 区块或复杂状态必须拆为独立子组件/hook，禁止继续堆高主文件。
- **性能与鉴权安全**：
  - 重型渲染依赖（如 `@xterm/xterm`、`mermaid`）必须按需动态懒加载，保护弱网首屏包体积。
  - 本地快照缓存（SWR）必须严格按会话 `scope` 隔离，鉴权通过前绝不展示任何历史正文，401/登出即清空。
- **前端验收门禁**：每次前端变更提交前必须全部通过：
  ```bash
  cd web && npm run typecheck && node --test tests/*.test.mjs && npm run build
  ```

## 4. 提交、契约与必测清单

- **约定式提交**：`feat:` / `fix:` / `refactor:` / `test:` / `docs:` / `chore:`。
- **契约先行**：新增或修改接口字段时，先同步 Go `internal/model` 与 `web/src/types/api.ts`，再实现前后端逻辑。
- **核心状态机必补表驱动单元测试**：
  - 原生大脑风控与防重：`hasHumanDecisionCardApproval`（`allow_release` / scope 升级）、非人类回合限制、跨线程隔离、`api_router_inflight` 崩溃对账、SSE 看门狗与 `reasoning_content` 透传；
  - 交互与配置闭环：Action 原子防重、决策卡选项校验、`manage_config` L2 确认卡与乐观哈希防并发覆写；
  - Worker 编排与运行时：无损休眠 GC 互斥、分层 `readonly` 沙箱、`agy` 双层防串话归属认领、审批卡片解析；
  - 基础底座：企微白名单与 quote/直聊路由、双路径富文本降级、`os.Root` 沙箱与跨机附件校验、定时器推进、SQLite 迁移。
