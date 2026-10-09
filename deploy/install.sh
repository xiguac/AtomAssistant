#!/usr/bin/env bash
# 在服务器上做一次性的安装：数据库、服务账号、目录、密钥、systemd 服务。
# 由 deploy.sh 上传并以 root 身份运行；可以重复运行，已经存在的东西不会被覆盖。
#
# 布局（程序和数据分开放）：
#   /opt/atomhelper/releases/<版本>/   程序的各个版本，current 链接指向正在用的那个
#   /var/lib/atomhelper/               数据：config/ 是配置，files/ 是文件工作区
#   /etc/atomhelper/env                密钥：数据库连接串、保险库密钥、后台口令摘要
#   Postgres 自己的数据目录             任务、记忆、保险库的密文
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "要用 root 运行（sudo bash install.sh）" >&2
  exit 1
fi

APP=/opt/atomhelper
DATA=/var/lib/atomhelper
ETC=/etc/atomhelper
ENV_FILE=$ETC/env
SRC=$(cd "$(dirname "$0")" && pwd)

echo "==> 安装 PostgreSQL（已安装则跳过）"
if ! command -v psql >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq postgresql >/dev/null
fi
systemctl enable --now postgresql >/dev/null

echo "==> 服务账号与目录"
id atomhelper >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin atomhelper
install -d -o root -g root -m 755 "$APP" "$APP/releases"
install -d -o atomhelper -g atomhelper -m 700 "$DATA" "$DATA/config" "$DATA/files"
install -d -o root -g atomhelper -m 750 "$ETC"

if [ ! -f "$ENV_FILE" ]; then
  echo "==> 生成密钥并建库"
  DB_PASS=$(openssl rand -hex 24)
  # 角色和库已存在时只重设密码：这是第一次生成 env，之前的密码已经无处可查。
  sudo -u postgres psql -v ON_ERROR_STOP=1 -q <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'atomhelper') THEN
    CREATE ROLE atomhelper LOGIN PASSWORD '$DB_PASS';
  ELSE
    ALTER ROLE atomhelper PASSWORD '$DB_PASS';
  END IF;
END \$\$;
SQL
  sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='atomhelper'" | grep -q 1 \
    || sudo -u postgres createdb -O atomhelper atomhelper

  umask 027
  cat > "$ENV_FILE" <<ENV
# 原子助手的密钥。root 可写，服务账号只读。不要把这个文件复制到别处或贴给别人。
ATOM_DSN=postgres://atomhelper:$DB_PASS@127.0.0.1:5432/atomhelper?sslmode=disable
# 保险库密钥：丢了它，保险库里的东西就再也解不开；迁移服务器时要连它一起带走。
ATOM_VAULT_KEY=$(openssl rand -base64 32)
ATOM_CONFIG=$DATA/config/config.yaml
ATOM_OWNER=atomd@$(hostname)
# 管理后台只听本机，经 SSH 隧道访问。要直接从公网访问，必须同时配置 TLS 证书：
#   ATOM_ADMIN_ADDR=0.0.0.0:443  ATOM_ADMIN_TLS_CERT=...  ATOM_ADMIN_TLS_KEY=...
ATOM_ADMIN_ADDR=127.0.0.1:8780
ATOM_ADMIN_PASSWORD_HASH=
ENV
  chown root:atomhelper "$ENV_FILE"
  chmod 640 "$ENV_FILE"
else
  echo "==> $ENV_FILE 已存在，保留原有的密钥"
fi

if [ ! -f "$DATA/config/config.yaml" ]; then
  echo "==> 写入初始配置（之后在管理后台里改）"
  install -o atomhelper -g atomhelper -m 600 "$SRC/config.initial.yaml" "$DATA/config/config.yaml"
fi

echo "==> 安装 systemd 服务"
install -o root -g root -m 644 "$SRC/atomhelper.service" /etc/systemd/system/atomhelper.service
systemctl daemon-reload
systemctl enable atomhelper >/dev/null 2>&1 || true

echo "==> 安装完成"
