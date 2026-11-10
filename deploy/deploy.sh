#!/usr/bin/env bash
# 在本机编译，把新版本的程序送上服务器并切换过去。数据不动。
#
# 用法（在仓库根目录、Git Bash 里）：
#   deploy/deploy.sh setup     第一次：装数据库、建目录和服务，然后部署
#   deploy/deploy.sh           之后每次更新：编译、上传、切换、检查，起不来就自动退回上一版
#   deploy/deploy.sh sandbox   安装代码沙箱：独立的 Python 环境与常用库，用 bubblewrap 隔离
#   deploy/deploy.sh asr       安装语音识别服务（聊天里的语音消息转成文字；识别程序在 ASR_DIR，默认 ../asr-1.0）
#   deploy/deploy.sh vector    安装向量记忆用的向量服务和 Qdrant（模型在 bin/embed，Qdrant 程序在 bin/qdrant-linux.tar.gz）
#   deploy/deploy.sh ip2region 安装公网 IP 归属地库（ip2region，只到市级；助手据此在定位缺失或离谱时判断你在哪个城市）
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

# ffmpeg 随程序一起发布：语音、视频在各聊天软件与模型之间转换格式要用它。
# 用 BtbN 编好的 Linux 版（带 opus、amr 编码器，只依赖系统自带的基础库），在本机下载一次缓存起来。
# 服务器上按内容摘要存一份（/opt/atomassistant/tools/ffmpeg-摘要），只在没有时上传；
# 每个版本目录里放一个指向它的链接，退回旧版本时 ffmpeg 跟着一起退。
FFMPEG_ASSET=ffmpeg-n8.1-latest-linux64-gpl-8.1
FFMPEG_URL=https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/$FFMPEG_ASSET.tar.xz

ffmpeg_local() {
  FFMPEG_BIN="bin/ffmpeg/$FFMPEG_ASSET/bin/ffmpeg"
  if [ ! -f "$FFMPEG_BIN" ]; then
    echo "==> 下载 ffmpeg（$FFMPEG_ASSET，只在第一次）"
    mkdir -p bin/ffmpeg
    curl -fsSL -m 900 -o "bin/ffmpeg/$FFMPEG_ASSET.tar.xz" "$FFMPEG_URL"
    tar -xJf "bin/ffmpeg/$FFMPEG_ASSET.tar.xz" -C bin/ffmpeg
  fi
  FFMPEG_SUM="$(sha256sum "$FFMPEG_BIN" | cut -c1-16)"
}

ffmpeg_remote() {
  local target="/opt/atomassistant/tools/ffmpeg-$FFMPEG_SUM"
  if remote "test -x $target"; then
    return 0
  fi
  echo "==> 上传 ffmpeg（只在它变了的时候）"
  gzip -c "$FFMPEG_BIN" > "bin/ffmpeg/ffmpeg.gz"
  "${SCP[@]}" "bin/ffmpeg/ffmpeg.gz" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-ffmpeg.gz"
  remote "set -e
    sudo install -d -o root -g root -m 755 /opt/atomassistant/tools
    gunzip -c /tmp/atomassistant-ffmpeg.gz | sudo tee $target.part >/dev/null
    rm -f /tmp/atomassistant-ffmpeg.gz
    sudo chmod 755 $target.part
    sudo mv $target.part $target
    $target -hide_banner -version | head -1"
}

healthy() {
  # 在服务器上问本机的后台：它只听 127.0.0.1。
  remote "for i in \$(seq 1 20); do curl -fsS -m 3 http://127.0.0.1:$ADMIN_PORT/healthz 2>/dev/null && exit 0; sleep 1; done; exit 1"
}

release() {
  build
  ffmpeg_local
  ffmpeg_remote
  echo "==> 上传"
  remote "rm -rf /tmp/atomassistant-upload && mkdir -p /tmp/atomassistant-upload"
  "${SCP[@]}" "$OUT/atomd" "$OUT/atomctl" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-upload/"
  echo "==> 切换到新版本"
  remote "set -e
    PREV=\$(readlink /opt/atomassistant/current 2>/dev/null || true)
    sudo install -d -o root -g root -m 755 /opt/atomassistant/releases/$VERSION
    sudo install -o root -g root -m 755 /tmp/atomassistant-upload/atomd /tmp/atomassistant-upload/atomctl /opt/atomassistant/releases/$VERSION/
    sudo ln -sfn /opt/atomassistant/tools/ffmpeg-$FFMPEG_SUM /opt/atomassistant/releases/$VERSION/ffmpeg
    rm -rf /tmp/atomassistant-upload
    echo \"\$PREV\" | sudo tee /opt/atomassistant/previous >/dev/null
    sudo ln -sfn /opt/atomassistant/releases/$VERSION /opt/atomassistant/current"
  # 第一次安装时后台口令还没设，服务起不来；先把程序放好，设完口令再启动。
  if [ "${NO_START:-}" = "1" ]; then
    echo "==> 程序已就位（尚未启动）"
    return 0
  fi
  remote "sudo systemctl restart atomassistant"
  echo "==> 检查"
  if result=$(healthy); then
    echo "    后台应答: $result"
    # 只留最近的几个版本，旧的删掉；正在用的和上一个永远留着。
    remote "cd /opt/atomassistant/releases && ls -1t | tail -n +$((KEEP + 1)) | while read -r old; do
      [ \"/opt/atomassistant/releases/\$old\" = \"\$(readlink /opt/atomassistant/current)\" ] && continue
      [ \"/opt/atomassistant/releases/\$old\" = \"\$(cat /opt/atomassistant/previous 2>/dev/null)\" ] && continue
      sudo rm -rf \"/opt/atomassistant/releases/\$old\"
    done"
    echo "==> 已部署 $VERSION"
  else
    echo "!! 新版本没有起来，退回上一版。最近的日志：" >&2
    remote "sudo journalctl -u atomassistant -n 30 --no-pager" >&2 || true
    rollback
    exit 1
  fi
}

rollback() {
  remote "set -e
    PREV=\$(cat /opt/atomassistant/previous 2>/dev/null || true)
    if [ -z \"\$PREV\" ] || [ ! -d \"\$PREV\" ]; then echo '没有可以退回的版本' >&2; exit 1; fi
    sudo ln -sfn \"\$PREV\" /opt/atomassistant/current
    sudo systemctl restart atomassistant
    echo \"已退回 \$PREV\""
}

case "${1:-release}" in
  setup)
    echo "==> 上传安装脚本"
    remote "rm -rf /tmp/atomassistant-setup && mkdir -p /tmp/atomassistant-setup"
    "${SCP[@]}" deploy/install.sh deploy/atomassistant.service deploy/config.initial.yaml "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-setup/"
    remote "sudo bash /tmp/atomassistant-setup/install.sh && rm -rf /tmp/atomassistant-setup"
    NO_START=1 release
    echo
    echo "程序和数据库都装好了。还差一步，设置后台的登录口令并启动："
    echo "  deploy/deploy.sh passwd"
    ;;
  scanner)
    echo "==> 上传扫描器的安装脚本"
    remote "rm -rf /tmp/atomassistant-scanner && mkdir -p /tmp/atomassistant-scanner"
    # 源码在本机下好再传上去：服务器直接连 GitHub 不一定连得上。
    SRC="$(mktemp -d)"
    trap 'rm -rf "$SRC"' EXIT
    curl -fsSL -m 300 -o "$SRC/skillspector.tar.gz" https://codeload.github.com/NVIDIA/skillspector/tar.gz/HEAD
    "${SCP[@]}" deploy/scanner.sh "$SRC/skillspector.tar.gz" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-scanner/"
    remote "sudo bash /tmp/atomassistant-scanner/scanner.sh && rm -rf /tmp/atomassistant-scanner"
    ;;
  sandbox)
    echo "==> 上传沙箱的安装脚本"
    remote "rm -rf /tmp/atomassistant-sandbox && mkdir -p /tmp/atomassistant-sandbox"
    "${SCP[@]}" deploy/sandbox.sh "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-sandbox/"
    remote "sudo bash /tmp/atomassistant-sandbox/sandbox.sh && rm -rf /tmp/atomassistant-sandbox && sudo systemctl restart atomassistant"
    ;;
  asr)
    # 只传识别程序、模型和自检用的音频；asr-1.0 里自带的 python/ 是 Windows 版，服务器上另建环境。
    ASR_DIR="${ASR_DIR:-../asr-1.0}"
    [ -f "$ASR_DIR/rt/webdemo.py" ] || { echo "在 $ASR_DIR 找不到识别程序（rt/webdemo.py），用 ASR_DIR 指定" >&2; exit 1; }
    echo "==> 打包识别程序和模型（$ASR_DIR）"
    PKG="$(mktemp -d)"
    trap 'rm -rf "$PKG"' EXIT
    tar -czf "$PKG/asr.tar.gz" --exclude=__pycache__ -C "$ASR_DIR" rt models samples selfcheck.py
    echo "==> 上传（约 150 MB）"
    remote "rm -rf /tmp/atomassistant-asr && mkdir -p /tmp/atomassistant-asr"
    "${SCP[@]}" deploy/asr.sh deploy/atomassistant-asr.service "$PKG/asr.tar.gz" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-asr/"
    remote "sudo bash /tmp/atomassistant-asr/asr.sh && rm -rf /tmp/atomassistant-asr && sudo systemctl restart atomassistant"
    ;;
  vector)
    MODEL_DIR=bin/embed/bge-small-zh-v1.5
    QDRANT_PKG=bin/qdrant-linux.tar.gz
    for f in "$MODEL_DIR/model.onnx" "$MODEL_DIR/model.safetensors" "$MODEL_DIR/tokenizer.json" "$QDRANT_PKG"; do
      [ -f "$f" ] || { echo "缺少 $f" >&2; exit 1; }
    done
    echo "==> 打包向量服务和模型"
    PKG="$(mktemp -d)"
    trap 'rm -rf "$PKG"' EXIT
    cp deploy/embed/embedd.py deploy/embed/check.py "$PKG/"
    cp -r "$MODEL_DIR" "$PKG/"
    tar -czf "$PKG/embed.tar.gz" -C "$PKG" embedd.py check.py bge-small-zh-v1.5
    echo "==> 上传（约 200 MB）"
    remote "rm -rf /tmp/atomassistant-vector && mkdir -p /tmp/atomassistant-vector"
    "${SCP[@]}" deploy/vector.sh deploy/atomassistant-embed.service deploy/atomassistant-qdrant.service       "$PKG/embed.tar.gz" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-vector/"
    "${SCP[@]}" "$QDRANT_PKG" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/atomassistant-vector/qdrant.tar.gz"
    remote "sudo bash /tmp/atomassistant-vector/vector.sh && rm -rf /tmp/atomassistant-vector && sudo systemctl restart atomassistant"
    ;;
  ip2region)
    # 在本机下载一次缓存起来，再传到服务器：服务器直接连 GitHub 不一定连得上。重启后生效。
    XDB_DIR=bin/ip2region
    XDB="$XDB_DIR/ip2region_v4.xdb"
    if [ ! -s "$XDB" ]; then
      echo "==> 下载 ip2region_v4.xdb（约 11 MB，只在第一次）"
      mkdir -p "$XDB_DIR"
      curl -fsSL -m 300 -o "$XDB.part" https://raw.githubusercontent.com/lionsoul2014/ip2region/master/data/ip2region_v4.xdb
      mv "$XDB.part" "$XDB"
    fi
    echo "==> 上传"
    "${SCP[@]}" "$XDB" "$DEPLOY_USER@$DEPLOY_HOST:/tmp/ip2region_v4.xdb"
    remote "set -e
      sudo install -d -o root -g root -m 755 /opt/atomassistant/data
      sudo install -o root -g root -m 644 /tmp/ip2region_v4.xdb /opt/atomassistant/data/ip2region_v4.xdb
      rm -f /tmp/ip2region_v4.xdb
      sudo systemctl restart atomassistant"
    if result=$(healthy); then
      echo "已安装并重启（$result）。"
    else
      echo "!! 服务没有起来。最近的日志：" >&2
      remote "sudo journalctl -u atomassistant -n 30 --no-pager" >&2 || true
      exit 1
    fi
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
      HASH=\$(sudo bash -c 'set -a; . /etc/atomassistant/env; set +a; /opt/atomassistant/current/atomctl admin passwd 2>/dev/null')
      [ -n \"\$HASH\" ] || { echo '生成摘要失败（口令太短？）' >&2; exit 1; }
      sudo sed -i '/^ATOM_ADMIN_PASSWORD_HASH=/d' /etc/atomassistant/env
      # 在后台页面里改过的密码存在这个文件里并且优先生效；这里重设时把它删掉，新设的才算数。
      sudo rm -f /var/lib/atomassistant/config/admin-password.hash
      printf 'ATOM_ADMIN_PASSWORD_HASH=%s\n' \"\$HASH\" | sudo tee -a /etc/atomassistant/env >/dev/null
      sudo systemctl restart atomassistant"
    unset PW PW2
    if result=$(healthy); then
      echo "口令已设置，服务已启动（$result）。"
      echo "用 deploy/deploy.sh tunnel 打开隧道，然后在浏览器里访问 http://localhost:$ADMIN_PORT"
    else
      echo "!! 口令已写入，但服务没有起来。最近的日志：" >&2
      remote "sudo journalctl -u atomassistant -n 30 --no-pager" >&2 || true
      exit 1
    fi
    ;;
  tunnel)
    # 地址用 localhost 而不是 127.0.0.1：通行密钥只认域名，不认 IP 地址。
    echo "隧道已打开：在浏览器里访问 http://localhost:$ADMIN_PORT  （按 Ctrl+C 关闭）"
    ssh -i "$DEPLOY_KEY" -N -L "$ADMIN_PORT:127.0.0.1:$ADMIN_PORT" "$DEPLOY_USER@$DEPLOY_HOST"
    ;;
  status)
    remote "systemctl status atomassistant --no-pager -n 0; echo; readlink /opt/atomassistant/current; echo; sudo journalctl -u atomassistant -n 25 --no-pager"
    ;;
  *)
    echo "未知的子命令 $1（可用：setup / asr / ip2region / scanner / sandbox / release / passwd / tunnel / status / rollback）" >&2
    exit 2
    ;;
esac
