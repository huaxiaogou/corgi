# Corgi 工程与代码规范

本文件是仓库的架构与代码红线，所有开发者与 AI Agent 在本仓库工作时必须遵守。

## 1. 架构分层与单向依赖

严禁破坏以下单向依赖关系，严禁出现循环引用：

```text
cmd/corgi
  ├── internal/cli
  └── internal/channel/{web,wecom}
        └── internal/core        (仅依赖接口，不 import channel/web 或 channel/wecom 实现)
              ├── internal/fleet
              ├── internal/runtime/{herdr,agent}
              └── internal/store
                    └── internal/model   (纯领域模型，零内部依赖)
```

## 2. 后端 Go 规范（Go 1.25+）

- **文件粒度**：单文件目标 **≤ 400 行**（核心状态机不超过 600 行），按单一职责拆分。
- **零可变全局状态**：禁止包级可变全局变量（缓存、连接、配置一律挂在结构体实例上），保证并发测试隔离与优雅停机。
- **并发与锁纪律**：
  - 所有外部 IO（`herdr` Unix Socket、yamux、企微 WSS、SQLite、子进程）必须透传 `context.Context` 并显式设置超时。
  - **锁内零 IO**：`sync.Mutex` 只保护纯内存字段；读取快照后立刻解锁，再做网络/文件/子进程调用。
- **错误处理与日志**：
  - 统一用 `fmt.Errorf("<动作上下文>: %w", err)` 包装错误，禁止静默吞错。
  - 统一使用标准库 `log/slog` 结构化日志。
  - **严禁在日志、错误响应或 URL 中泄露任何明文 Token 或企微 Secret**。
- **数据库与安全**：
  - SQLite WAL 走显式版本迁移（`PRAGMA user_version`），一律使用单行增量参数化 SQL，禁止整表全量覆写。
  - 文件接口严格规范化路径与大小限制；HTML 预览强制 CSP 沙箱。
  - 企微通道接入层严格执行 `allow_user_ids` 发送者白名单校验，非白名单一律静默丢弃。
- **验收门禁**：每次提交前必须全部通过：
  ```bash
  gofmt -l .
  go vet ./...
  go test -race ./...
  ```

## 3. 前端规范（Vite + React + TypeScript + Tailwind CSS）

- **视觉铁律（极冷峻黑白纸面风）**：
  - 所有颜色唯一定义在 `web/src/styles/tokens.css` 语义变量（`--bg / --bg-2 / --bg-3 / --line / --line-2 / --fg / --fg-2 / --fg-3 / --accent / --on-accent / --attn / --attn-soft / --ok`）。
  - **禁止在组件中硬编码 hex 颜色、禁止高饱和 Tailwind 彩色类（如 `bg-blue-*`、`bg-purple-*`、`bg-gradient-*`）**。
  - Tailwind 仅用于布局、间距、字号、圆角和响应式；全站 99% 黑白暖灰 + 仅 1% 琥珀提醒点。
- **TypeScript 严格模式与组件边界**：
  - `tsconfig.json` 保持 `"strict": true`、`noUnusedLocals`、`noUnusedParameters`，禁止随意 `any`。
  - 前后端契约集中在 `web/src/types/api.ts`。
  - 按 `components/`（纯 UI 原语）、`features/`（业务模块）、`hooks/`（SSE/状态）、`lib/`（API 客户端）拆分，单组件目标 **≤ 200 行**。
- **验收门禁**：每次前端变更必须全部通过：
  ```bash
  cd web && npm run typecheck && npm run build
  ```

## 4. 提交与契约约定

- 约定式提交：`feat:` / `fix:` / `refactor:` / `test:` / `docs:` / `chore:`。
- 新增接口字段先同步 Go `internal/model` 与 `web/src/types/api.ts`，再实现业务逻辑。
- 关键状态机（Action 原子防重、企微白名单与 quote 话题路由、定时器推进、SQLite 迁移、审批卡片解析）必须补表驱动单元测试。
