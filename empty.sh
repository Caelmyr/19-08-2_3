#!/bin/bash
set -e

# ========== 初始化 Git 仓库并推送到 GitHub ==========
read -p "请输入 GitHub 仓库名称 (例如 gy-45): " REPO_NAME
COMMIT_MSG="init"  # 提交消息，可以是实际的 session ID
# ================================================

# 目标文件夹默认为当前目录
TARGET_DIR="$(pwd)"
echo
echo "Target folder: $TARGET_DIR"
echo "Repo name   : $REPO_NAME"
echo "Commit msg  : $COMMIT_MSG"
echo

# 如果存在旧的 .git 文件夹，先删除重新初始化
if [ -d ".git" ]; then
    echo "Removing existing .git folder..."
    rm -rf .git
fi

# 初始化 Git 仓库，分支为 main
git init -b main
if [ $? -ne 0 ]; then
    echo "[ERROR] git init failed"
    exit 1
fi

# 检查文件夹是否为空（除了隐藏文件）
hasFiles=$(ls -A | wc -l)
if [ "$hasFiles" -eq 0 ]; then
    echo "Folder is empty, creating README.md..."
    echo "# $REPO_NAME" > README.md
fi

# 创建 .gitignore 文件（如果不存在）
if [ ! -f ".gitignore" ]; then
    cat > .gitignore << 'EOF'
node_modules/
__pycache__/
*.pyc
.env
EOF
fi

# 将自身脚本添加到 .gitignore（不提交到仓库）
SCRIPT_NAME="$(basename "$0")"
echo "$SCRIPT_NAME" >> .gitignore

# 添加所有文件并提交
git add .
git commit -m "$COMMIT_MSG"
if [ $? -ne 0 ]; then
    echo "[ERROR] git commit failed"
    exit 1
fi

# 使用 gh 创建 GitHub 仓库并推送（需要 gh CLI 和已登录）
gh repo create "$REPO_NAME" --public --source=. --push
if [ $? -ne 0 ]; then
    echo "[ERROR] gh repo create failed"
    exit 1
fi

# 显示 Commit ID
echo
echo "========================================"
echo "Repo URL  : https://github.com/$(gh api user --jq '.login')/$REPO_NAME"
echo "Commit ID :"
git log --format=%H -1
echo "========================================"
echo "Done."
