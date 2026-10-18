#!/usr/bin/env bash
# 在本机编译，把新版本的程序送上服务器并切换过去。数据不动。
#
# 用法（在仓库根目录、Git Bash 里）：
#   deploy/deploy.sh setup     第一次：装数据库、建目录和服务，然后部署
#   deploy/deploy.sh           之后每次更新：编译、上传、切换、检查，起不来就自动退回上一版
#   deploy/deploy.sh sandbox   安装代码沙箱：独立的 Python 环境与常用库，用 bubblewrap 隔离
#   deploy/deploy.sh media     安装 ffmpeg：语音、视频在各聊天软件与模型之间转换格式要用它
#   deploy/deploy.sh scanner   安装 Skill 扫描器 SkillSpector（助手自己安装 Skill、MCP server 之前要用它扫描）
#   deploy/deploy.sh passwd    设置管理后台的登录口令（口令在你的终端里输入，不经过任何文件）
#   deploy/deploy.sh tunnel    打开到管理后台的 SSH 隧道，然后在浏览器里访问 http://localhost:8780
#   deploy/deploy.sh status    看服务状态和最近的日志
#   deploy/deploy.sh rollback  退回上一个版本
#
# 服务器地址和密钥写在 deploy/deploy.env 里（不进版本库），格式见 deploy/deploy.env.example。
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -f deploy/deploy.env ]; then
  echo "缺少 deploy/deploy.env，照 deploy/deploy.env.example 写一份" >&2
  exit 1
fi
# shellcheck disable=SC1091
source deploy/deploy.env
: "${DEPLOY_HOST:?deploy.env 里要有 DEPLOY_HOST}" "${DEPLOY_USER:?要有 DEPLOY_USER}" "${DEPLOY_KEY:?要有 DEPLOY_KEY}"

# 不自动接受变化了的主机密钥：密钥对不上时宁可停下来让人看一眼。
SSH=(ssh -i "$DEPLOY_KEY" -o BatchMode=yes -o ConnectTimeout=15 "$DEPLOY_USER@$DEPLOY_HOST")
SCP=(scp -q -i "$DEPLOY_KEY" -o BatchMode=yes -o ConnectTimeout=15)
KEEP=5
ADMIN_PORT=8780

remote() { "${SSH[@]}" "$@"; }

build() {
  VERSION="$(date +%Y%m%d-%H%M%S)-$(git rev-parse --short HEAD)"
  if [ -n "$(git status --porcelain)" ]; then
    VERSION="$VERSION-dirty"
  fi
  OUT="bin/release/$VERSION"
  mkdir -p "$OUT"
  echo "==> 编译 $VERSION（linux/amd64）"
  for cmd in atomd atomctl; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$OUT/$cmd" "./cmd/$cmd"
  done
}

healthy() {
  # 在服务器上问本机的后台：它只听 127.0.0.1。
  remote "for i in \$(seq 1 20); do curl -fsS -m 3 http://127.0.0.1:$ADMIN_PORT/healthz 2>/dev/null && exit 0; sleep 1; done; exit 1"
}

release() {
  build
  echo "==> 上传"
  remote "rm -rf /tmp/atomhelper-upload && mkdir -p /tmp/atomhelper-upload"
  "${SCP[@]}" "$OUT/atomd" "$OUT/atomctl" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomhelper-upload/"
  echo "==> 切换到新版本"
  remote "set -e
    PREV=\$(readlink /opt/atomhelper/current 2>/dev/null || true)
    sudo install -d -o root -g root -m 755 /opt/atomhelper/releases/$VERSION
    sudo install -o root -g root -m 755 /tmp/atomhelper-upload/atomd /tmp/atomhelper-upload/atomctl /opt/atomhelper/releases/$VERSION/
    rm -rf /tmp/atomhelper-upload
    echo \"\$PREV\" | sudo tee /opt/atomhelper/previous >/dev/null
    sudo ln -sfn /opt/atomhelper/releases/$VERSION /opt/atomhelper/current"
  # 第一次安装时后台口令还没设，服务起不来；先把程序放好，设完口令再启动。
  if [ "${NO_START:-}" = "1" ]; then
    echo "==> 程序已就位（尚未启动）"
    return 0
  fi
  remote "sudo systemctl restart atomhelper"
  echo "==> 检查"
  if result=$(healthy); then
    echo "    后台应答: $result"
    # 只留最近的几个版本，旧的删掉；正在用的和上一个永远留着。
    remote "cd /opt/atomhelper/releases && ls -1t | tail -n +$((KEEP + 1)) | while read -r old; do
      [ \"/opt/atomhelper/releases/\$old\" = \"\$(readlink /opt/atomhelper/current)\" ] && continue
      [ \"/opt/atomhelper/releases/\$old\" = \"\$(cat /opt/atomhelper/previous 2>/dev/null)\" ] && continue
      sudo rm -rf \"/opt/atomhelper/releases/\$old\"
    done"
    echo "==> 已部署 $VERSION"
  else
    echo "!! 新版本没有起来，退回上一版。最近的日志：" >&2
    remote "sudo journalctl -u atomhelper -n 30 --no-pager" >&2 || true
    rollback
    exit 1
  fi
}

rollback() {
  remote "set -e
    PREV=\$(cat /opt/atomhelper/previous 2>/dev/null || true)
    if [ -z \"\$PREV\" ] || [ ! -d \"\$PREV\" ]; then echo '没有可以退回的版本' >&2; exit 1; fi
    sudo ln -sfn \"\$PREV\" /opt/atomhelper/current
    sudo systemctl restart atomhelper
    echo \"已退回 \$PREV\""
}

case "${1:-release}" in
  setup)
    echo "==> 上传安装脚本"
    remote "rm -rf /tmp/atomhelper-setup && mkdir -p /tmp/atomhelper-setup"
    "${SCP[@]}" deploy/install.sh deploy/atomhelper.service deploy/config.initial.yaml "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomhelper-setup/"
    remote "sudo bash /tmp/atomhelper-setup/install.sh && rm -rf /tmp/atomhelper-setup"
    NO_START=1 release
    echo
    echo "程序和数据库都装好了。还差一步，设置后台的登录口令并启动："
    echo "  deploy/deploy.sh passwd"
    ;;
  scanner)
    echo "==> 上传扫描器的安装脚本"
    remote "rm -rf /tmp/atomhelper-scanner && mkdir -p /tmp/atomhelper-scanner"
    # 源码在本机下好再传上去：服务器直接连 GitHub 不一定连得上。
    SRC="$(mktemp -d)"
    trap 'rm -rf "$SRC"' EXIT
    curl -fsSL -m 300 -o "$SRC/skillspector.tar.gz" https://codeload.github.com/NVIDIA/skillspector/tar.gz/HEAD
    "${SCP[@]}" deploy/scanner.sh "$SRC/skillspector.tar.gz" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomhelper-scanner/"
    remote "sudo bash /tmp/atomhelper-scanner/scanner.sh && rm -rf /tmp/atomhelper-scanner"
    ;;
  sandbox)
    echo "==> 上传沙箱的安装脚本"
    remote "rm -rf /tmp/atomhelper-sandbox && mkdir -p /tmp/atomhelper-sandbox"
    "${SCP[@]}" deploy/sandbox.sh "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomhelper-sandbox/"
    remote "sudo bash /tmp/atomhelper-sandbox/sandbox.sh && rm -rf /tmp/atomhelper-sandbox && sudo systemctl restart atomhelper"
    ;;
  media)
    echo "==> 安装 ffmpeg"
    remote "sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ffmpeg >/dev/null && ffmpeg -hide_banner -version | head -1"
    remote "sudo systemctl restart atomhelper"
    ;;
  release) release ;;
  rollback) rollback ;;
  passwd)
    # 口令在本机终端里输入，经 SSH 的加密通道交给服务器上的 atomctl 算摘要，
    # 摘要写进 env 文件。口令本身不落在任何文件里，也不出现在命令行上。
    read -r -s -p "设置管理后台的登录口令（至少 10 个字符）: " PW; echo
    read -r -s -p "再输一遍: " PW2; echo
    [ "$PW" = "$PW2" ] || { echo "两次输入不一样" >&2; exit 1; }
    printf '%s\n' "$PW" | remote "set -e
      HASH=\$(sudo bash -c 'set -a; . /etc/atomhelper/env; set +a; /opt/atomhelper/current/atomctl admin passwd 2>/dev/null')
      [ -n \"\$HASH\" ] || { echo '生成摘要失败（口令太短？）' >&2; exit 1; }
      sudo sed -i '/^ATOM_ADMIN_PASSWORD_HASH=/d' /etc/atomhelper/env
      # 在后台页面里改过的密码存在这个文件里并且优先生效；这里重设时把它删掉，新设的才算数。
      sudo rm -f /var/lib/atomhelper/config/admin-password.hash
      printf 'ATOM_ADMIN_PASSWORD_HASH=%s\n' \"\$HASH\" | sudo tee -a /etc/atomhelper/env >/dev/null
      sudo systemctl restart atomhelper"
    unset PW PW2
    if result=$(healthy); then
      echo "口令已设置，服务已启动（$result）。"
      echo "用 deploy/deploy.sh tunnel 打开隧道，然后在浏览器里访问 http://localhost:$ADMIN_PORT"
    else
      echo "!! 口令已写入，但服务没有起来。最近的日志：" >&2
      remote "sudo journalctl -u atomhelper -n 30 --no-pager" >&2 || true
      exit 1
    fi
    ;;
  tunnel)
    # 地址用 localhost 而不是 127.0.0.1：通行密钥只认域名，不认 IP 地址。
    echo "隧道已打开：在浏览器里访问 http://localhost:$ADMIN_PORT  （按 Ctrl+C 关闭）"
    ssh -i "$DEPLOY_KEY" -N -L "$ADMIN_PORT:127.0.0.1:$ADMIN_PORT" "$DEPLOY_USER@$DEPLOY_HOST"
    ;;
  status)
    remote "systemctl status atomhelper --no-pager -n 0; echo; readlink /opt/atomhelper/current; echo; sudo journalctl -u atomhelper -n 25 --no-pager"
    ;;
  *)
    echo "未知的子命令 $1（可用：setup / media / scanner / sandbox / release / passwd / tunnel / status / rollback）" >&2
    exit 2
    ;;
esac
