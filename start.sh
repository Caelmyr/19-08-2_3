#!/usr/bin/env bash
# =============================================================================
# CoEdit 一键启动脚本（前后端 + 数据库）
#
# 前端（web/static 静态页面）由 Go 服务端同进程托管，因此"启动前后端"=
# 准备好 MySQL + 建表 + 编译并启动 Go 服务端。启动后浏览器打开首页即可协作。
#
# 用法：
#   ./start.sh                # 默认 8080 端口，MySQL root/root@127.0.0.1:3306/coedit
#   ./start.sh --port 9090    # 自定义端口
#   环境变量 DB_HOST DB_PORT DB_USER DB_PASS DB_NAME 可覆盖数据库连接
#   ./start.sh --stop         # 停止本脚本启动的服务进程
#
# 注意：scripts/schema.sql 会重建表（DROP + CREATE），重复启动会清空已有数据。
# =============================================================================
set -euo pipefail

# -----------------------------------------------------------------------------
# 0. 定位项目根目录（脚本所在目录），并解析参数
# -----------------------------------------------------------------------------
cd "$(dirname "$(readlink -f "$0")")"
PROJECT_ROOT="$(pwd)"

PORT="${PORT:-8080}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-3306}"
DB_USER="${DB_USER:-root}"
DB_PASS="${DB_PASS:-root}"
DB_NAME="${DB_NAME:-coedit}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --port)
      PORT="${2:-8080}"; shift 2 ;;
    --port=*)
      PORT="${1#*=}"; shift ;;
    --stop)
      STOP=1; shift ;;
    *)
      echo "未知参数: $1"; exit 1 ;;
  esac
done

BIN="./bin/coedit-server"
PID_FILE="./bin/coedit.pid"

# -----------------------------------------------------------------------------
# 停止模式
# -----------------------------------------------------------------------------
if [[ "${STOP:-0}" == "1" ]]; then
  if [[ -f "$PID_FILE" ]]; then
    kill "$(cat "$PID_FILE")" 2>/dev/null && echo "已停止 CoEdit（PID $(cat "$PID_FILE")）" || echo "进程已不存在"
    rm -f "$PID_FILE"
  else
    pkill -f "$PROJECT_ROOT/bin/coedit-server" 2>/dev/null && echo "已停止 CoEdit" || echo "没有正在运行的 CoEdit 进程"
  fi
  exit 0
fi

# -----------------------------------------------------------------------------
# 1. 检查 Go 工具链
# -----------------------------------------------------------------------------
if ! command -v go >/dev/null 2>&1; then
  echo "错误：未找到 Go，请先安装 Go（项目要求 1.26.x）"
  exit 1
fi
echo "✅ Go 工具链: $(go version)"

# -----------------------------------------------------------------------------
# 2. 检查 / 准备 MySQL
# -----------------------------------------------------------------------------
mysql_ping() {
  mysqladmin ping -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" -p"$DB_PASS" --silent 2>/dev/null
}

if mysql_ping; then
  echo "✅ MySQL 已就绪（$DB_HOST:$DB_PORT）"
else
  echo "⚠️  无法连接 MySQL，尝试本机服务或 Docker 兜底启动……"
  # 兜底 1：本机已安装 mysqld 且数据目录已初始化
  if command -v mysqld >/dev/null 2>&1 && [[ -d /var/lib/mysql/mysql ]]; then
    echo "  发现本机 mysqld，尝试拉起……"
    (service mysql start 2>/dev/null || mysqld_safe --skip-grant-tables=0 >/tmp/coedit-mysqld.log 2>&1 &)
  # 兜底 2：用 Docker 起一个 MySQL
  elif command -v docker >/dev/null 2>&1; then
    echo "  用 Docker 启动 MySQL（容器名 coedit-mysql）……"
    docker run -d --name coedit-mysql \
      -e MYSQL_ROOT_PASSWORD="$DB_PASS" \
      -e MYSQL_DATABASE="$DB_NAME" \
      -p "$DB_PORT":3306 \
      mysql:8.0 >/dev/null 2>&1 || echo "  Docker 容器可能已存在，跳过创建"
  fi

  # 最多等待 30 秒
  for _ in $(seq 1 30); do
    mysql_ping && break
    sleep 1
  done
  if ! mysql_ping; then
    echo "错误：MySQL 仍不可用。请手动启动 MySQL，或用 DB_HOST/DB_PORT/DB_USER/DB_PASS 指定连接信息"
    exit 1
  fi
  echo "✅ MySQL 已就绪"
fi

# -----------------------------------------------------------------------------
# 3. 初始化数据库表结构
# -----------------------------------------------------------------------------
echo "📦 初始化数据库表结构（$DB_NAME）……"
if [[ -n "$DB_PASS" ]]; then
  mysql -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" -p"$DB_PASS" < scripts/schema.sql
else
  mysql -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" < scripts/schema.sql
fi
echo "✅ 表结构就绪"

# -----------------------------------------------------------------------------
# 4. 编译服务端
# -----------------------------------------------------------------------------
echo "🔨 编译服务端……"
mkdir -p bin
go build -o "$BIN" ./cmd/server
echo "✅ 编译完成"

# -----------------------------------------------------------------------------
# 5. 启动服务端（前台，Ctrl+C 停止）
# -----------------------------------------------------------------------------
echo ""
echo "=============================================================="
echo "  CoEdit 启动中"
echo "  首页:     http://localhost:$PORT"
echo "  编辑器:   http://localhost:$PORT/editor.html?doc_id=<文档ID>"
echo "  健康检查: http://localhost:$PORT/health"
echo "  按 Ctrl+C 停止服务"
echo "=============================================================="
echo ""

"$BIN" \
  -port "$PORT" \
  -db-host "$DB_HOST" \
  -db-port "$DB_PORT" \
  -db-user "$DB_USER" \
  -db-pass "$DB_PASS" \
  -db-name "$DB_NAME" &
SERVER_PID=$!
echo "$SERVER_PID" > "$PID_FILE"
trap 'echo ""; echo "正在停止 CoEdit……"; kill "$SERVER_PID" 2>/dev/null; rm -f "$PID_FILE"; exit 0' INT TERM

# 等待服务就绪
for _ in $(seq 1 20); do
  if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
    echo "✅ 服务已就绪，打开 http://localhost:$PORT 开始协作"
    break
  fi
  sleep 0.5
done

wait "$SERVER_PID"
