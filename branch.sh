#!/bin/bash
set -e

# ============================================
#  Git 多仓库分支创建脚本
#  在当前项目目录运行，创建 x 个独立 Git 仓库
#  本地：每个仓库只有 A、B 两个分支目录和 .git（不含项目代码）
#  远端：master 含当前目录代码（commit: init），A、B 为空分支
# ============================================

if ! git rev-parse --git-dir > /dev/null 2>&1; then
    echo "[ERROR] 当前目录不是 Git 仓库"
    exit 1
fi

ORIG_DIR="$(pwd)"
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

# 从远程 URL 提取仓库名作为基础名称
REMOTE_URL="$(git remote get-url origin 2>/dev/null || echo "")"
if [[ -n "$REMOTE_URL" ]]; then
    BASE_NAME="$(basename "$REMOTE_URL" .git)"
    BASE_NAME="${BASE_NAME##*/}"
else
    BASE_NAME="$(basename "$(pwd)")"
    echo "[WARN] 未找到远程 origin，使用目录名: $BASE_NAME"
fi

echo "当前分支: $CURRENT_BRANCH"
echo "远程仓库: $REMOTE_URL"
echo "基础名称: $BASE_NAME"
echo

read -p "请输入要创建的仓库数量: " COUNT

if ! [[ "$COUNT" =~ ^[0-9]+$ ]] || [ "$COUNT" -le 0 ]; then
    echo "[ERROR] 请输入大于 0 的正整数"
    exit 1
fi

PARENT_DIR="$(dirname "$ORIG_DIR")"
WORKTREE_ROOT="${PARENT_DIR}/git_${BASE_NAME}"
mkdir -p "$WORKTREE_ROOT"

echo
echo "仓库存放目录: $WORKTREE_ROOT"
echo "将基于当前代码创建 $COUNT 个独立仓库："
echo "============================================================"

SUCCESS=0
FAIL=0
SKIP=0

# 获取当前仓库的远程用户名（用于创建新仓库）
if [[ -n "$REMOTE_URL" ]]; then
    REMOTE_USER="$(echo "$REMOTE_URL" | sed -E 's|.*github\.com[:/]([^/]+)/.*|\1|')"
    if [[ -z "$REMOTE_USER" ]]; then
        echo "[ERROR] 无法从远程 URL 提取用户名"
        exit 1
    fi
    echo "远程用户: $REMOTE_USER"
    echo
fi

for ((i=1; i<=COUNT; i++)); do
    REPO_NAME="${BASE_NAME}_${i}"
    REPO_DIR="${WORKTREE_ROOT}/${REPO_NAME}"

    echo "[${i}/${COUNT}] 创建仓库 '$REPO_NAME' ..."

    # 已存在则先删除本地目录和远程仓库，保证干净重建
    if [ -d "$REPO_DIR" ]; then
        rm -rf "$REPO_DIR"
        echo "  → 已删除本地旧目录"
    fi
    if command -v gh &> /dev/null && gh repo view "$REMOTE_USER/$REPO_NAME" >/dev/null 2>&1; then
        gh repo delete "$REMOTE_USER/$REPO_NAME" --yes
        echo "  → 已删除远程旧仓库"
    fi

    # 1. 创建本地 Git 仓库
    mkdir -p "$REPO_DIR"
    cd "$REPO_DIR"
    git init -b master

    # 2. 复制当前目录代码并提交 init（代码用于推送到远程 master）
    rsync -a --exclude='.git' --exclude='.gitignore' "$ORIG_DIR/" "$REPO_DIR/"
    git add .
    git commit -m "init"

    # 3. 创建远程仓库并设置 remote
    if command -v gh &> /dev/null; then
        if gh repo create "$REMOTE_USER/$REPO_NAME" --private --source=. --push; then
            echo "✓ 远程仓库创建成功"
            # 确保 remote origin 使用 HTTPS（gh token 走 HTTPS；SSH 在此环境不通）
            git remote set-url origin "https://github.com/${REMOTE_USER}/${REPO_NAME}.git"
        else
            echo "[WARN] 远程仓库创建失败，跳过远程推送"
        fi
    else
        echo "[WARN] gh CLI 未安装，跳过远程仓库创建"
    fi

    # 4. 创建本地 A 和 B 分支（orphan：无提交，初始代码只进工作区、不 commit）
    BRANCH_DIR_A="${WORKTREE_ROOT}/${REPO_NAME}/A"
    BRANCH_DIR_B="${WORKTREE_ROOT}/${REPO_NAME}/B"

    # 创建 A 分支：orphan 工作区，把 master 的初始代码放入工作区（仅暂存，不提交）
    if git worktree add --orphan -b A "$BRANCH_DIR_A" 2>/dev/null; then
        git -C "$BRANCH_DIR_A" checkout master -- .
        echo "✓ A 分支创建成功（初始代码已放入，等待容器修改后首次提交）"
    else
        echo "[WARN] 创建 A 分支失败"
    fi

    # 创建 B 分支：同上
    if git worktree add --orphan -b B "$BRANCH_DIR_B" 2>/dev/null; then
        git -C "$BRANCH_DIR_B" checkout master -- .
        echo "✓ B 分支创建成功（初始代码已放入，等待容器修改后首次提交）"
    else
        echo "[WARN] 创建 B 分支失败"
    fi

    # 5. 不再预建远程 A/B —— 由 commit.sh 首次 push 时创建
    #    （远程 A/B 的第一个提交，就是容器里修改后提交的那份代码）
    cd "$REPO_DIR"

    echo "✓ 完成"
    ((SUCCESS += 1))
done

echo "============================================================"
echo "完成！成功: ${SUCCESS}, 跳过: ${SKIP}, 失败: ${FAIL}"
echo
echo "📂 目录结构："
for ((i=1; i<=COUNT; i++)); do
    REPO_NAME="${BASE_NAME}_${i}"
    if [ -d "${WORKTREE_ROOT}/${REPO_NAME}" ]; then
        echo "   ${WORKTREE_ROOT}/${REPO_NAME}/"
        echo "     ├── A/   (A 分支)"
        echo "     └── B/   (B 分支)"
    fi
done

echo
echo "🔗 远程仓库："
for ((i=1; i<=COUNT; i++)); do
    REPO_NAME="${BASE_NAME}_${i}"
    echo "   $REMOTE_USER/$REPO_NAME"
    echo "     ├── master (含代码)"
    echo "     ├── A (空分支)"
    echo "     └── B (空分支)"
done

echo
echo "💡 下一步：进入 A 或 B 分支目录，运行 commit.sh 提交代码"
echo "   代码将自动推送到对应的远程仓库和分支"
