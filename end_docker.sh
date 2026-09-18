#!/bin/bash
set -euo pipefail

# ============================================================
#  end_docker.sh - 导出轨迹并删除本题容器（第二版流程）
#
#  在 run_docker.sh 的容器停止后执行：导出轨迹到本机
#  $RUN_DIR/traces，确认代码与轨迹完整后删除容器。
#
#  用法：
#    ./end_docker.sh                  # 默认容器名为当前项目目录名
#    ./end_docker.sh claude-b         # 指定容器名（多开时与启动一致）
# ============================================================

CONTAINER_NAME="${CONTAINER_NAME:-${1:-$(basename "$(dirname "$PWD")")_$(basename "$PWD")}}"

# 1. 确认容器存在
if ! docker ps -a --format '{{.Names}}' | grep -wq "$CONTAINER_NAME"; then
    echo "❌ 容器 $CONTAINER_NAME 不存在，可能已删除或名称不一致。"
    echo "   请确认与 run_docker.sh 使用的容器名一致。"
    exit 1
fi

# 2. 从容器挂载推导 RUN_DIR（与 run_docker.sh 保持一致，无需手动记录变量）
WORKSPACE_SRC="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}' "$CONTAINER_NAME")"
if [ -z "$WORKSPACE_SRC" ]; then
    echo "❌ 无法从容器 $CONTAINER_NAME 读取 /workspace 挂载源。"
    exit 1
fi
RUN_DIR="$(dirname "$WORKSPACE_SRC")"
TRACES_DIR="$RUN_DIR/traces"

echo "🐳 容器：$CONTAINER_NAME"
echo "📂 本题本地目录：$RUN_DIR"
echo "📁 代码目录：$RUN_DIR/workspace"

# 3. 导出轨迹（保留完整目录结构）
echo ""
echo "📋 导出轨迹到 $TRACES_DIR ..."
mkdir -p "$TRACES_DIR"

EXPORT_OK=0
if docker cp "$CONTAINER_NAME:/home/node/.claude/projects" "$TRACES_DIR" 2>/dev/null; then
    EXPORT_OK=1
    echo "✅ 轨迹已导出到 $TRACES_DIR"
else
    echo "⚠️  轨迹目录不存在或为空，可能尚未产生对话记录。"
    echo "   请确认至少完成了一次对话；导出报错时先解决并重新导出，不要删除容器。"
fi

# 4. 显示找到的 Session ID
echo ""
echo "📋 找到的 Session ID："
SESSION_IDS="$(find "$TRACES_DIR" -name "*.jsonl" -type f 2>/dev/null | sort -u)"
if [ -n "$SESSION_IDS" ]; then
    echo "$SESSION_IDS" | while read -r f; do
        SID="$(basename "$f" .jsonl)"
        echo "  - $SID"
    done
else
    echo "  （未找到 .jsonl 会话文件）"
fi

# 5. 确认代码与轨迹完整后删除容器
echo ""
if [ "$EXPORT_OK" -eq 1 ]; then
    read -r -p "确认本机代码与轨迹已完整，删除容器 $CONTAINER_NAME？[y/N] " CONFIRM
else
    echo "轨迹导出不完整，默认不删除，请先排查。"
    read -r -p "若确认无需轨迹，仍要删除容器 $CONTAINER_NAME？[y/N] " CONFIRM
fi

if [[ "$CONFIRM" =~ ^[Yy]$ ]]; then
    docker rm "$CONTAINER_NAME"
    echo "✅ 容器已删除，本机 $RUN_DIR 目录不受影响。"
else
    echo "已保留容器 $CONTAINER_NAME，可稍后重新执行本脚本或手动 docker rm $CONTAINER_NAME。"
fi
