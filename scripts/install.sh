#!/bin/sh
# Corgi one-command installer (binary + herdr + agent integrations + daemon service)
#
#   1. 仅安装 corgi 命令并准备 herdr：
#        sh scripts/install.sh
#
#   2. 一条命令把这台电脑做成 Hub 并后台常驻（Linux systemd --user / 容器自动 nohup / macOS launchd）：
#        sh scripts/install.sh hub [--url http://<本机内网 IP>:7788]
#
#   3. 一条命令把这台电脑作为 Node 加入 Hub 并后台常驻（同步开放 http://127.0.0.1:17788 独立 PWA）：
#        sh scripts/install.sh node --hub http://<hub>:7788 --code <8位配对码> [--name macbook]
#
#   4. 使用预编译二进制或远程一键安装：
#        CORGI_BINARY=./dist/corgi-linux-amd64 sh scripts/install.sh hub --url http://<ip>:7788
#        curl -fsSL https://raw.githubusercontent.com/huaxiaogou/corgi/main/scripts/install.sh | sh -s -- hub
#
# 环境变量：
#   CORGI_BIN_DIR      安装目录（默认 ~/.local/bin）
#   CORGI_BINARY       直接安装指定二进制文件（跳过编译与下载）
#   CORGI_REPO         GitHub 仓库（默认 huaxiaogou/corgi）
#   CORGI_VERSION      下载版本（默认 latest）
#   CORGI_NO_HERDR     设为 1 跳过自动安装 herdr
#   CORGI_DRY_SERVICE  设为 1 仅打印服务配置不写入（调试/自测用）
set -eu

REPO=${CORGI_REPO:-huaxiaogou/corgi}
VERSION=${CORGI_VERSION:-latest}
BIN_DIR=${CORGI_BIN_DIR:-$HOME/.local/bin}
BIN=$BIN_DIR/corgi

say()  { printf '%s\n' "$*"; }
step() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\033[31mcorgi:\033[0m %s\n' "$*" >&2; exit 1; }

# ------------------------------------------------------------------ platform
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) die "暂时只支持 macOS 和 Linux（当前：$(uname -s)）" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64)  arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "暂时只支持 amd64 和 arm64（当前：$(uname -m)）" ;;
esac

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1"
  else
    die "需要 curl 或 wget"
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

build_service_path() {
  p="$BIN_DIR:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
  node_dir=""
  if command -v node >/dev/null 2>&1; then
    node_dir=$(dirname "$(command -v node)")
  fi
  for extra in "$HOME/.cargo/bin" "$HOME/go/bin" "$HOME/.npm-global/bin" "$HOME/.volta/bin" "$node_dir" /opt/homebrew/opt/node*/bin; do
    [ -n "$extra" ] && [ -d "$extra" ] || continue
    case ":$p:" in
      *":$extra:"*) ;;
      *) p="$extra:$p" ;;
    esac
  done
  printf '%s' "$p"
}

# ------------------------------------------------------------------ binary
checkout() {
  case "$0" in *install.sh) ;; *) return 1 ;; esac
  d=$(cd "$(dirname "$0")/.." 2>/dev/null && pwd) || return 1
  grep -q '^module github.com/kiperjing/corgi$' "$d/go.mod" 2>/dev/null || return 1
  printf '%s\n' "$d"
}

ensure_shell_path() {
  # If /usr/local/bin is writable (e.g. root on Linux), link corgi/herdr there so
  # the current shell can run `corgi` immediately without even re-sourcing rc files.
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ] && [ "$BIN_DIR" != "/usr/local/bin" ]; then
    ln -sf "$BIN" /usr/local/bin/corgi
    [ -x "$BIN_DIR/herdr" ] && ln -sf "$BIN_DIR/herdr" /usr/local/bin/herdr
  fi

  case ":$PATH:" in
    *":$BIN_DIR:"*) return 0 ;;
  esac

  line="export PATH=\"$BIN_DIR:\$PATH\""
  updated=""
  for rc in "$HOME/.zshrc" "$HOME/.bashrc"; do
    [ -f "$rc" ] || continue
    if ! grep -Fq "$BIN_DIR" "$rc" 2>/dev/null; then
      printf '\n# Added by Corgi installer\n%s\n' "$line" >> "$rc"
      updated="$updated $rc"
    fi
  done
  if [ -z "$updated" ]; then
    case "${SHELL:-}" in
      */zsh)  target_rc="$HOME/.zshrc" ;;
      */bash) target_rc="$HOME/.bashrc" ;;
      *)      target_rc="$HOME/.profile" ;;
    esac
    if ! grep -Fq "$BIN_DIR" "$target_rc" 2>/dev/null; then
      printf '\n# Added by Corgi installer\n%s\n' "$line" >> "$target_rc"
      updated=" $target_rc"
    fi
  fi
  if [ -n "$updated" ]; then
    step "已自动将 $BIN_DIR 写入$updated"
    if [ ! -L /usr/local/bin/corgi ]; then
      say "    当前终端若未立即生效，执行一次：export PATH=\"$BIN_DIR:\$PATH\""
    fi
  fi
}

install_binary() {
  asset=corgi-$os-$arch
  if [ "$VERSION" = latest ]; then
    base=https://github.com/$REPO/releases/latest/download
  else
    base=https://github.com/$REPO/releases/download/$VERSION
  fi
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT

  if [ -n "${CORGI_BINARY:-}" ]; then
    [ -f "$CORGI_BINARY" ] || die "找不到指定的二进制文件：$CORGI_BINARY"
    step "安装指定二进制：$CORGI_BINARY"
    cp "$CORGI_BINARY" "$tmp/$asset"
  elif src=$(checkout); then
    if [ -x "$src/dist/$asset" ] && [ -z "${CORGI_FORCE_BUILD:-}" ]; then
      step "使用本地构建产物（$src/dist/$asset）"
      cp "$src/dist/$asset" "$tmp/$asset"
    else
      command -v go >/dev/null 2>&1 || die "从源码编译需要 Go 1.25+：https://go.dev/dl/"
      if [ ! -f "$src/web/dist/index.html" ]; then
        command -v npm >/dev/null 2>&1 || die "首次构建前端需要 Node.js / npm"
        step "构建内嵌前端资源"
        (cd "$src/web" && npm install --no-audit --no-fund && npm run build) || die "前端构建失败"
      fi
      v=$(git -C "$src" describe --tags --always --dirty 2>/dev/null || echo dev)
      step "从源码编译（$src，$v）"
      (cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$v" -o "$tmp/$asset" ./cmd/corgi) || die "Go 编译失败"
    fi
  else
    step "下载 $asset（$VERSION）"
    fetch "$base/$asset" "$tmp/$asset" || die "下载失败：$base/$asset"
    if fetch "$base/SHA256SUMS.txt" "$tmp/SHA256SUMS.txt" 2>/dev/null; then
      want=$(grep " $asset\$" "$tmp/SHA256SUMS.txt" | awk '{print $1}')
      [ -n "$want" ] && [ "$want" = "$(sha256 "$tmp/$asset")" ] || die "SHA-256 校验和不匹配，已中止安装"
    fi
  fi

  mkdir -p "$BIN_DIR"
  chmod 755 "$tmp/$asset"
  mv -f "$tmp/$asset" "$BIN.new" && mv -f "$BIN.new" "$BIN"
  step "已安装 $("$BIN" version 2>/dev/null || echo corgi) → $BIN"
  if [ -z "${CORGI_DRY_SERVICE:-}" ]; then
    ensure_shell_path
  fi
}

# ------------------------------------------------------------------ daemon service
xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }

install_service() {
  name=$1; shift
  svc_path=$(build_service_path)

  if [ "$os" = darwin ]; then
    label=com.corgi.$name
    plist=$HOME/Library/LaunchAgents/$label.plist
    log=$HOME/Library/Logs/corgi-$name.log
    args="    <string>$(xml "$BIN")</string>"
    for a in "$@"; do
      args="$args
    <string>$(xml "$a")</string>"
    done
    unit="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key>
  <array>
$args
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>$(xml "$svc_path")</string></dict>
  <key>WorkingDirectory</key><string>$(xml "$HOME")</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>15</integer>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$(xml "$log")</string>
  <key>StandardErrorPath</key><string>$(xml "$log")</string>
</dict>
</plist>"
    if [ -n "${CORGI_DRY_SERVICE:-}" ]; then say "$unit"; return; fi
    mkdir -p "$(dirname "$plist")" "$(dirname "$log")"
    printf '%s\n' "$unit" > "$plist"
    launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$plist"
    step "已作为后台服务常驻（launchd: $label）"
    say "    日志：tail -f $log"
  else
    unitdir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
    file=$unitdir/corgi-$name.service
    quoted=""
    for a in "$@"; do quoted="$quoted \"$a\""; done
    unit="[Unit]
Description=Corgi $name
After=network-online.target

[Service]
ExecStart=\"$BIN\"$quoted
WorkingDirectory=%h
Environment=PATH=$svc_path
Restart=always
RestartSec=3

[Install]
WantedBy=default.target"
    if [ -n "${CORGI_DRY_SERVICE:-}" ]; then say "$unit"; return; fi

    if ! command -v systemctl >/dev/null 2>&1 || ! systemctl --user show-environment >/dev/null 2>&1; then
      log=$HOME/.corgi/$name.log
      pidf=$HOME/.corgi/$name.pid
      mkdir -p "$HOME/.corgi"
      if [ -f "$pidf" ] && kill "$(cat "$pidf")" 2>/dev/null; then sleep 1; fi
      PATH=$svc_path nohup "$BIN" "$@" >>"$log" 2>&1 </dev/null &
      echo $! > "$pidf"
      sleep 2
      kill -0 "$(cat "$pidf")" 2>/dev/null || die "后台启动失败，请检查日志：tail -n 30 $log"
      step "已在后台运行（容器/无 systemd 环境自动采用 nohup；pid $(cat "$pidf")）"
      say "    日志：tail -f $log"
      return
    fi

    mkdir -p "$unitdir"
    printf '%s\n' "$unit" > "$file"
    systemctl --user stop "corgi-$name" 2>/dev/null || true
    systemctl --user reset-failed "corgi-$name" 2>/dev/null || true
    systemctl --user daemon-reload
    systemctl --user enable --now "corgi-$name" >/dev/null
    systemctl --user restart "corgi-$name"
    step "已作为后台服务常驻（systemd --user: corgi-$name）"
    say "    日志：journalctl --user -u corgi-$name -f"
    if command -v loginctl >/dev/null 2>&1 && ! loginctl show-user "$(id -un)" -p Linger 2>/dev/null | grep -q yes; then
      if loginctl enable-linger "$(id -un)" 2>/dev/null; then
        step "已自动开启 loginctl enable-linger（断开 SSH 后继续后台常驻）"
      else
        say "    提示：退出 SSH 后保持常驻请运行一次：loginctl enable-linger"
      fi
    fi
  fi
}

# ------------------------------------------------------------------ herdr & agent integrations
setup_herdr() {
  if [ -n "${CORGI_DRY_SERVICE:-}" ]; then return 0; fi
  svc_path=$(build_service_path)
  hb=$(PATH="$svc_path" command -v herdr 2>/dev/null || true)

  if [ -z "$hb" ] && [ -z "${CORGI_NO_HERDR:-}" ]; then
    step "自动安装 herdr（Corgi 底层终端引擎；跳过可设 CORGI_NO_HERDR=1）"
    if curl -fsSL https://herdr.dev/install.sh | HERDR_INSTALL_DIR="$BIN_DIR" sh >/dev/null 2>&1; then
      hb=$BIN_DIR/herdr
      [ -d /usr/local/bin ] && [ -w /usr/local/bin ] && [ "$BIN_DIR" != "/usr/local/bin" ] && ln -sf "$hb" /usr/local/bin/herdr
      step "herdr 安装完成 → $hb"
    else
      warn "herdr 自动安装未成功，可稍后手动执行：curl -fsSL https://herdr.dev/install.sh | sh"
    fi
  fi
  if [ -z "$hb" ] || [ ! -x "$hb" ]; then
    warn "未找到 herdr：这台电脑上的 Agent 将暂时无法由 Corgi 驱动${1:+（$1）}。"
    return 0
  fi

  if ! "$hb" status 2>/dev/null | grep -q "status: running"; then
    PATH="$svc_path" nohup "$hb" server >/dev/null 2>&1 </dev/null &
    sleep 2
    if "$hb" status 2>/dev/null | grep -q "status: running"; then
      step "已在后台启动 herdr server"
    else
      warn "herdr server 未就绪，请手动运行一次：herdr server"
    fi
  fi

  st=$("$hb" integration status 2>/dev/null) || return 0
  for pair in claude:claude codex:codex agy:antigravity-cli; do
    cmd_name=${pair%%:*}
    target=${pair#*:}
    PATH="$svc_path" command -v "$cmd_name" >/dev/null 2>&1 || continue
    line=$(printf '%s\n' "$st" | grep "^$target:" || true)
    case "$line" in
      "" | *": current"*) ;;
      *)
        step "自动安装 herdr 集成：$target"
        if ! "$hb" integration install "$target" >/dev/null 2>&1; then
          warn "请手动执行：herdr integration install $target"
        fi
        ;;
    esac
  done
  if PATH="$svc_path" command -v codebuddy >/dev/null 2>&1; then
    say "    已检测到 codebuddy（Corgi 原生适配 + 自动终端兜底，无需额外集成）"
  fi
}

# ------------------------------------------------------------------ main
mode=${1:-}
[ $# -gt 0 ] && shift

case "$mode" in
  "")
    install_binary
    setup_herdr
    say ""
    say "下一步（均支持一条命令后台常驻）："
    say "  做 Hub：   sh scripts/install.sh hub --url http://<本机内网 IP>:7788"
    say "  做 Node：  sh scripts/install.sh node --hub http://<hub>:7788 --code <8位配对码>"
    ;;
  hub)
    install_binary
    install_service hub serve "$@"
    say "    扫码连接手机：corgi pair（或在 Web「自动化与设置 → 设备与多机」扫码）"
    say "    生成电脑加入码：corgi pair --computer"
    setup_herdr "Hub 本身仍可正常调度其他已连入的电脑"
    ;;
  node)
    install_binary
    case " $* " in
      *" --code "* | *" --code="*)
        step "配对并加入 Hub"
        "$BIN" node --pair-only "$@"
        ;;
      *)
        [ -f "$HOME/.corgi/node.json" ] || die "首次加入请带上 Hub 地址和配对码：sh scripts/install.sh node --hub http://<hub>:7788 --code <8位配对码>"
        ;;
    esac
    # Keep only persistent node flags (--name / --socket / --ui) in daemon arguments.
    n=$#
    i=0
    while [ "$i" -lt "$n" ]; do
      a=$1; shift; i=$((i + 1))
      case "$a" in
        --name | --socket | --ui)
          [ "$i" -lt "$n" ] && { set -- "$@" "$a" "$1"; shift; i=$((i + 1)); }
          ;;
        --name=* | --socket=* | --ui=*)
          set -- "$@" "$a"
          ;;
        --hub | --code)
          [ "$i" -lt "$n" ] && { shift; i=$((i + 1)); }
          ;;
        *) ;;
      esac
    done
    install_service node node "$@"
    say "    本机独立 PWA 地址：http://127.0.0.1:17788（Chrome 地址栏右侧可直接安装到 Dock）"
    setup_herdr
    ;;
  *)
    die "未知模式：$mode（支持 hub、node 或留空仅安装命令）"
    ;;
esac
