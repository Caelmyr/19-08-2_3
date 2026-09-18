#!/bin/bash
set -euo pipefail

# ============================================================
#  run_docker.sh - 启动本题 Claude Code 容器
#
#  一份镜像，每道题新建一个容器，容器内统一使用 /workspace。
#  流程：在当前项目下新建 workspace -> 启动容器并挂载 workspace
#        -> 把当前分支 clone 成 workspace/<项目名>/（独立 git 仓库，
#        origin 指向远程）-> 删除已迁移的原项目文件 -> 进入容器做题
#        -> 退出后在宿主机 workspace/<项目名>/ 内用 commit.sh 提交推送。
#
#  用法：
#    ./run_docker.sh                  # 默认容器名为「上一级目录名_当前目录名」
#    ./run_docker.sh claude-b         # 指定容器名（多开时使用）
#    CONTAINER_NAME=claude-a ./run_docker.sh   # 通过环境变量指定容器名
#    API_KEY=xxxxx ./run_docker.sh    # 通过环境变量传入 Key
# ============================================================

# ========== 配置区 ==========
API_KEY="sk-aXGlkuf_QdWMhVJLrica4A"
IMAGE="adminfather/benzhi-claude-code:20260909-isolated-git"
CONTAINER_NAME="${CONTAINER_NAME:-${1:-$(basename "$(dirname "$PWD")")_$(basename "$PWD")}}"
# =============================

# 1. 检查容器是否已存在
if docker ps -a --format '{{.Names}}' | grep -wq "$CONTAINER_NAME"; then
    if docker ps --format '{{.Names}}' | grep -wq "$CONTAINER_NAME"; then
        echo "⏳ 容器已在运行，直接进入对话..."
        docker exec -it "$CONTAINER_NAME" bash -lc "claude --resume || claude"
        exit 0
    else
        echo "⚠️  容器 $CONTAINER_NAME 已存在但未运行。"
        echo "   若需重建，请先执行: docker rm $CONTAINER_NAME"
        exit 1
    fi
fi

# 2. 获取 API Key
if [ -z "$API_KEY" ]; then
    read -rsp "请输入 API Key: " API_KEY
    echo
    if [ -z "$API_KEY" ]; then
        echo "❌ API Key 不能为空"
        exit 1
    fi
fi
echo "⚠️  Key 会保存在终端历史和本题容器配置中，请勿分享含真实 Key 的命令或截图。"

# 3. 清空并重建 workspace 目录（保证为空，满足容器启动检查）
rm -rf "$PWD/workspace"
mkdir -p "$PWD/workspace"

# 4. 后台启动容器（空 /workspace 通过入口检查）
echo "🚀 创建容器 $CONTAINER_NAME ..."
docker run -dit --init \
    --restart=no \
    --cap-drop ALL \
    --security-opt no-new-privileges \
    --name "$CONTAINER_NAME" \
    --mount "type=bind,src=$PWD/workspace,dst=/workspace" \
    -e "apikey=$API_KEY" \
    "$IMAGE"

# 等待容器完全启动
sleep 1

# 5. 把当前分支 clone 成 workspace/<项目名>/（独立 git 仓库，origin 指向远程）
PROJECT_DIR="$(basename "$PWD")"
BRANCH="$(git branch --show-current)"
REMOTE_URL="$(git remote get-url origin 2>/dev/null || true)"
echo "📂 克隆分支 $BRANCH 到 workspace/$PROJECT_DIR/ ..."
if git clone -q -b "$BRANCH" "$(git rev-parse --git-common-dir)" "$PWD/workspace/$PROJECT_DIR"; then
    :
else
    # 当前分支尚无提交（orphan 分支没有 refs/heads/<BRANCH>），无法 clone -b。
    # 先 clone 默认分支拿到初始代码，再重建 orphan 分支并暂存，等价于 branch.sh 里 A/B 的初始状态。
    echo "   ℹ️  分支 $BRANCH 尚无提交（orphan），改用 clone + checkout --orphan 重建……"
    git clone -q "$(git rev-parse --git-common-dir)" "$PWD/workspace/$PROJECT_DIR"
    git -C "$PWD/workspace/$PROJECT_DIR" checkout --orphan "$BRANCH"
    git -C "$PWD/workspace/$PROJECT_DIR" add .
fi
if [ -n "$REMOTE_URL" ]; then
    git -C "$PWD/workspace/$PROJECT_DIR" remote set-url origin "$REMOTE_URL"
fi

# 6. 删除当前目录下已迁移的项目文件（保留 workspace/、run_docker.sh、end_docker.sh 及隐藏文件）
echo "🗑️  清理已迁移的项目文件..."
for item in "$PWD"/*; do
    name="$(basename "$item")"
    if [ "$name" = "workspace" ] || [ "$name" = "run_docker.sh" ] || [ "$name" = "end_docker.sh" ]; then
        continue
    fi
    rm -rf "$item"
done

# 7. 进入容器
echo "🎯 进入容器..."
docker attach "$CONTAINER_NAME"

echo ""
echo "容器 $CONTAINER_NAME 已退出，代码保存在 $PWD/workspace/$PROJECT_DIR。"
echo "提交代码（宿主机执行）："
echo "  cd $PWD/workspace/$PROJECT_DIR && ./commit.sh"
