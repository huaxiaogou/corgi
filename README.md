# Corgi

**1 个大脑（常驻 Super-Agent）+ 多台电脑手脚（Node）+ 企业微信（单聊 & 群聊多话题）+ 极冷峻黑白纸面 Web/PWA**。

基于 `herdr` 驱动 Claude Code、Codex、Antigravity（`agy`）与 CodeBuddy（`codebuddy`），让你在手机和电脑上用一句话指挥一群编程 Agent 干活，只在需要拍板时出声。

## 快速开始（一条命令全包安装）

一条命令自动完成：**安装 `corgi` + 自动安装并拉起 `herdr server` + 自动安装已装 Agent 的 `herdr` 集成 + 注册后台常驻服务**（Linux `systemd --user` / 容器自动 `nohup` / macOS `launchd`）。

```bash
# 1. 常开 Linux 开发机做 Hub（一条命令）
sh scripts/install.sh hub --url http://<linux-hub-ip>:7788
# 若直接传预编译二进制（免装 Go/Node）：
# CORGI_BINARY=./corgi-linux-amd64 sh scripts/install.sh hub --url http://<linux-hub-ip>:7788

# 2. 在 Hub 获取手机配对二维码 / 电脑加入码
corgi pair               # 手机扫码（或浏览器登录）
corgi pair --computer    # 8 位一次性电脑加入码

# 3. MacBook 或其他电脑作为 Node 加入（一条命令，自动开放 http://127.0.0.1:17788 安装 Dock PWA）
sh scripts/install.sh node --hub http://<linux-hub-ip>:7788 --code <8位配对码> --name macbook

# 4. 交叉编译四平台单二进制与 SHA256SUMS.txt
VERSION=v0.1.0 sh scripts/build-release.sh
```

> 若从源码手动编译：需 Go 1.25+ 与 Node.js 20+（运行期单二进制零依赖）。`codebuddy`（`cbc`）由 Corgi 自动兼容，无需额外 `herdr` 集成。

### 验证测试
```bash
go vet ./...
go test -race ./...
cd web && npm run typecheck && npm run build
```

### 常驻能力与外部接入
- **记忆自治**：`memory/` 分层目录（10 KB 首屏预算）+ 上下文达阈值自动 **Handover** 交接换会话 + 每日定时/手动 **Dream** 复盘日记与 Skill 提炼建议。
- **外部 Webhook**：支持 `POST /hooks/github?key=<hook_key>` 与通用 `POST /hooks/<source>?key=<hook_key>`（202 异步入箱、幂等去重、`respond/candidate/ignore` 快路径过滤），并可用 `corgi hook <source> "<text>"` 本地联调。
- **Outbox 节流**：审批/决策卡实时同步企微与 Web；普通回复默认 20 秒合批推送并在浏览网页时安静防打扰。
