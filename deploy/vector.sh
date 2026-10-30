#!/usr/bin/env bash
# 在服务器上安装向量记忆用的两个本机服务：向量服务（把文字编码成向量）和 Qdrant（存向量、做检索）。
#
# 由 deploy/deploy.sh vector 调用，以 root 身份在服务器上运行。同目录里要有：
#   embed.tar.gz                      embedd.py、check.py 和模型目录 bge-small-zh-v1.5/
#   qdrant.tar.gz                     Qdrant 的 Linux 版程序（官方发布的 x86_64 musl 版）
#   atomassistant-embed.service、atomassistant-qdrant.service
# 两个服务都只听 127.0.0.1。Qdrant 要 API key，由这里生成，写进 /etc/atomassistant 下的 env 文件，不经用户之手。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
EMBED=/opt/atomassistant-embed
QDRANT=/opt/atomassistant-qdrant
CONFIG=/var/lib/atomassistant/config/config.yaml
ENV=/etc/atomassistant/env
QENV=/etc/atomassistant/qdrant.env
EMBED_PORT=8766
QDRANT_PORT=6333
# 服务器在国内，PyPI 官方源慢且不稳，用腾讯云的镜像。
INDEX=${EMBED_PIP_INDEX:-https://mirrors.tencentyun.com/pypi/simple}

echo "==> 安装 Python 虚拟环境支持"
apt-get update -qq
apt-get install -y -qq python3-venv curl >/dev/null

echo "==> 向量服务：独立的 Python 环境 $EMBED/venv"
install -d -o root -g root -m 755 "$EMBED"
if [ ! -x "$EMBED/venv/bin/python3" ]; then
  python3 -m venv "$EMBED/venv"
fi
"$EMBED/venv/bin/python3" -m pip install -q --upgrade pip -i "$INDEX"
"$EMBED/venv/bin/python3" -m pip install -q -i "$INDEX" numpy onnxruntime tokenizers

rm -rf "$EMBED/app.new"
mkdir -p "$EMBED/app.new"
tar -xzf "$HERE/embed.tar.gz" -C "$EMBED/app.new"
chown -R root:root "$EMBED"
chmod -R go-w,go+rX "$EMBED"

# ONNX 模型是第三方从官方权重转出来的：用另一份独立的 numpy 实现对照官方权重算一遍，对得上才装。
echo "==> 核对 ONNX 模型与官方权重是否一致"
if ! (cd "$EMBED/app.new" && PYTHONIOENCODING=utf-8 "$EMBED/venv/bin/python3" check.py "$EMBED/app.new/bge-small-zh-v1.5"); then
  echo "!! 模型核对没通过，新版本不装，旧的照常运行" >&2
  rm -rf "$EMBED/app.new"
  exit 1
fi
rm -f "$EMBED/app.new/bge-small-zh-v1.5/model.safetensors"   # 只核对用，服务不需要
systemctl stop atomassistant-embed 2>/dev/null || true
rm -rf "$EMBED/app.old"
[ -d "$EMBED/app" ] && mv "$EMBED/app" "$EMBED/app.old"
mv "$EMBED/app.new" "$EMBED/app"

echo "==> Qdrant：程序放到 $QDRANT"
install -d -o root -g root -m 755 "$QDRANT"
tar -xzf "$HERE/qdrant.tar.gz" -C "$QDRANT"
chown root:root "$QDRANT/qdrant"
chmod 755 "$QDRANT/qdrant"

# API key 只生成一次：之后再装沿用原来的，向量库里已有的数据照样能用。
if [ ! -s "$QENV" ]; then
  echo "==> 生成 Qdrant 的 API key"
  KEY=$(head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 40)
  umask 077
  printf 'QDRANT__SERVICE__API_KEY=%s\n' "$KEY" > "$QENV"
  chmod 600 "$QENV"
fi
KEY=$(sed -n 's/^QDRANT__SERVICE__API_KEY=//p' "$QENV")
sed -i '/^ATOM_QDRANT_KEY=/d' "$ENV"
printf 'ATOM_QDRANT_KEY=%s\n' "$KEY" >> "$ENV"

THREADS=$(nproc)
[ "$THREADS" -gt 2 ] && THREADS=2

echo "==> 安装并启动两个服务"
sed "s/@THREADS@/$THREADS/" "$HERE/atomassistant-embed.service" > /etc/systemd/system/atomassistant-embed.service
install -m 644 "$HERE/atomassistant-qdrant.service" /etc/systemd/system/atomassistant-qdrant.service
systemctl daemon-reload
systemctl enable -q atomassistant-embed atomassistant-qdrant
systemctl restart atomassistant-embed atomassistant-qdrant

wait_for() {
  local what=$1 unit=$2; shift 2
  for _ in $(seq 1 60); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "!! $what 一分钟内没有就绪。最近的日志：" >&2
  journalctl -u "$unit" -n 30 --no-pager >&2 || true
  exit 1
}
wait_for 向量服务 atomassistant-embed curl -fsS -m 3 "http://127.0.0.1:$EMBED_PORT/healthz"
wait_for Qdrant atomassistant-qdrant curl -fsS -m 3 -H "api-key: $KEY" "http://127.0.0.1:$QDRANT_PORT/collections"
# 不带 key 应被拒：确认鉴权确实开着。
if curl -fsS -m 3 "http://127.0.0.1:$QDRANT_PORT/collections" >/dev/null 2>&1; then
  echo "!! Qdrant 不带 API key 也能访问，鉴权没有生效" >&2
  exit 1
fi
rm -rf "$EMBED/app.old"

if [ -f "$CONFIG" ] && ! grep -q '^memory_vector:' "$CONFIG"; then
  echo "==> 在配置里启用向量记忆"
  printf '\n# ---- 向量记忆（deploy.sh vector 写入） ----\nmemory_vector:\n  embed_url: http://127.0.0.1:%s\n  qdrant_url: http://127.0.0.1:%s\n' \
    "$EMBED_PORT" "$QDRANT_PORT" >> "$CONFIG"
fi
echo "==> 向量记忆的服务已就绪，重启助手后生效"
