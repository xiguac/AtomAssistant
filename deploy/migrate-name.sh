#!/usr/bin/env bash
# 程序改名之后在服务器上做一次的迁移：服务、账号、目录、数据库都换成新名字。
# 由 deploy.sh migrate-name 上传并以 root 身份运行；中途失败可以重跑，做过的步骤会跳过。
# 只停服务、改名字，不启动主服务：换新程序、重新加密数据、启动由 deploy.sh 接着做。
# 迁移完成后，这个脚本连同 deploy.sh 里的 migrate-name 一起删掉。
set -euo pipefail

if [ "$(id -u)" -ne 0 ]; then
  echo "要用 root 运行（sudo bash migrate-name.sh）" >&2
  exit 1
fi

OLD=atomhelper
NEW=atomassistant
UNITS=/etc/systemd/system
# 改动之前的数据库、密钥文件、env 和配置都先留一份，确认新版本跑好了再删。
BAK=/var/backups/$NEW-rename

echo "==> 停掉旧服务"
for unit in "$OLD" "$OLD-asr"; do
  if [ -f "$UNITS/$unit.service" ]; then
    systemctl stop "$unit" || true
    systemctl disable -q "$unit" || true
  fi
done

echo "==> 备份到 $BAK"
install -d -o root -g root -m 700 "$BAK"
if [ ! -f "$BAK/db.dump" ] && sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='$OLD'" | grep -q 1; then
  sudo -u postgres pg_dump -Fc "$OLD" > "$BAK/db.dump.part"
  mv "$BAK/db.dump.part" "$BAK/db.dump"
fi
for f in "/etc/$OLD/env" "/var/lib/$OLD/config/config.yaml" "/var/lib/$OLD/config/$OLD.key"; do
  [ -f "$f" ] && [ ! -f "$BAK/$(basename "$f")" ] && cp -p "$f" "$BAK/"
done
chmod -R go-rwx "$BAK"

echo "==> 服务账号"
if id "$OLD" >/dev/null 2>&1; then
  usermod -l "$NEW" "$OLD"
fi
if getent group "$OLD" >/dev/null; then
  groupmod -n "$NEW" "$OLD"
fi
usermod -d "/var/lib/$NEW" "$NEW"

echo "==> 目录"
for src in "/opt/$OLD" "/var/lib/$OLD" "/etc/$OLD" "/opt/$OLD-sandbox" "/opt/$OLD-asr"; do
  dst="${src/$OLD/$NEW}"
  if [ -e "$src" ] && [ -e "$dst" ]; then
    echo "!! $src 和 $dst 同时存在，不知道该用哪个，停下来" >&2
    exit 1
  fi
  [ -e "$src" ] && mv "$src" "$dst"
done
# 版本目录的 current 链接、每个版本里的 ffmpeg 链接都写的是绝对路径。
find "/opt/$NEW" -type l | while read -r link; do
  target=$(readlink "$link")
  case "$target" in
    "/opt/$OLD/"*) ln -sfn "/opt/$NEW/${target#/opt/$OLD/}" "$link" ;;
  esac
done
[ -f "/opt/$NEW/previous" ] && sed -i "s#^/opt/$OLD/#/opt/$NEW/#" "/opt/$NEW/previous"

echo "==> env 与配置里的名字"
sed -i "s/$OLD/$NEW/g" "/etc/$NEW/env"
[ -f "/var/lib/$NEW/config/config.yaml" ] && sed -i "s#/$OLD#/$NEW#g" "/var/lib/$NEW/config/config.yaml"

echo "==> 数据库"
psql_q() { sudo -u postgres psql -v ON_ERROR_STOP=1 -q "$@"; }
if sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='$OLD'" | grep -q 1; then
  psql_q -c "ALTER DATABASE $OLD RENAME TO $NEW"
fi
if sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='$OLD'" | grep -q 1; then
  psql_q -c "ALTER ROLE $OLD RENAME TO $NEW"
fi
# 角色改名会清掉 MD5 方式存的密码，按 env 里的连接串重设一遍。
DB_PASS=$(sed -n "s#^ATOM_DSN=postgres://$NEW:\([^@]*\)@.*#\1#p" "/etc/$NEW/env")
if [ -z "$DB_PASS" ]; then
  echo "!! 从 /etc/$NEW/env 的 ATOM_DSN 里取不到数据库密码，请手动核对后重设角色 $NEW 的密码" >&2
  exit 1
fi
psql_q <<SQL
ALTER ROLE $NEW PASSWORD '$DB_PASS';
SQL

echo "==> systemd 服务"
if [ -f "$UNITS/$OLD-asr.service" ]; then
  sed "s/$OLD/$NEW/g" "$UNITS/$OLD-asr.service" > "$UNITS/$NEW-asr.service"
  rm -f "$UNITS/$OLD-asr.service"
fi
rm -f "$UNITS/$OLD.service"
systemctl daemon-reload
if [ -f "$UNITS/$NEW-asr.service" ]; then
  systemctl enable -q "$NEW-asr"
  systemctl restart "$NEW-asr"
fi

echo "==> 改名完成（主服务尚未启动）"
