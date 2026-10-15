#Requires -Version 5.1
# 一键发布 phanthywork2api 到 GitHub
# 用法：在项目目录运行 .\publish.ps1

$ErrorActionPreference = "Stop"
$repoPath = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $repoPath

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  phanthywork2api GitHub 一键发布脚本" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan

# ---- 检查前置条件 ----
Write-Host "`n[1/5] 检查工作区..." -ForegroundColor Yellow

$status = git status --porcelain 2>$null
if ($status) {
    Write-Host "❌ 工作区有未提交改动，请先提交:" -ForegroundColor Red
    Write-Host $status
    exit 1
}
Write-Host "✅ 工作区干净" -ForegroundColor Green

$branch = git rev-parse --abbrev-ref HEAD 2>$null
if ($branch -ne "main") {
    Write-Host "⚠️ 当前分支是 $branch，建议切到 main 再发布" -ForegroundColor Yellow
    $ans = Read-Host "是否切换到 main? [Y/n]"
    if ($ans -eq "" -or $ans -match "^[Yy]") {
        git checkout main
    }
}

# ---- 输入 Token（安全输入，不显示）----
Write-Host "`n[2/5] GitHub Personal Access Token" -ForegroundColor Yellow
Write-Host "   生成地址: https://github.com/settings/tokens"
Write-Host "   需要权限: repo + write:packages (推镜像到 ghcr.io)"
$token = Read-Host "请输入你的 PAT (输入时不显示)" -AsSecureString
$plainToken = [Runtime.InteropServices.Marshal]::PtrToStringAuto([Runtime.InteropServices.Marshal]::SecureStringToBSTR($token))

if ($plainToken -notmatch "^ghp_[a-zA-Z0-9]{36}$") {
    Write-Host "⚠️ Token 格式看起来不对，应为 ghp_ 开头 + 36 位字符" -ForegroundColor Yellow
    $cont = Read-Host "仍要继续? [y/N]"
    if ($cont -notmatch "^[Yy]") { exit 0 }
}

# ---- 设置远程并推送 ----
Write-Host "`n[3/5] 推送到 GitHub..." -ForegroundColor Yellow

$remoteUrl = "https://somthingthere:${plainToken}@github.com/somthingthere/phanthywork2api.git"
git remote remove origin 2>$null
git remote add origin $remoteUrl

try {
    git push -u origin main
    Write-Host "✅ 源码推送成功!" -ForegroundColor Green
} catch {
    Write-Host "❌ 推送失败: $_" -ForegroundColor Red
    Write-Host "   常见原因: token 无 repo 权限 / 仓库名不对 / 网络问题"
    git remote remove origin 2>$null
    exit 1
}

# ---- 可选：发版 ----
Write-Host "`n[4/5] 是否立即发版?" -ForegroundColor Yellow
Write-Host "   发版会打 tag 并推送，触发 GitHub Actions 自动构建二进制和 Docker 镜像"
$doRelease = Read-Host "输入版本号发版 (如 1.0.0)，或直接回车跳过"

if ($doRelease) {
    $ver = $doRelease -replace "^v", ""
    $tag = "v${ver}"

    if ($ver -notmatch "^\d+\.\d+\.\d+$") {
        Write-Host "❌ 版本号格式不对，应为 X.Y.Z" -ForegroundColor Red
        exit 1
    }

    if (git rev-parse "$tag" 2>$null) {
        Write-Host "❌ 标签 $tag 已存在" -ForegroundColor Red
        exit 1
    }

    git tag -a "$tag" -m "Release $tag"
    git push origin "$tag"
    Write-Host "✅ 已推送标签 $tag" -ForegroundColor Green
    Write-Host "   查看构建进度: https://github.com/somthingthere/phanthywork2api/actions" -ForegroundColor Cyan
}

# ---- 清理 ----
Write-Host "`n[5/5] 清理临时 remote..." -ForegroundColor Yellow
git remote remove origin 2>$null

# 重新添加不带 token 的 remote（便于后续正常使用）
git remote add origin "https://github.com/somthingthere/phanthywork2api.git" 2>$null

Write-Host "`n========================================" -ForegroundColor Green
Write-Host "  发布完成!" -ForegroundColor Green
Write-Host "========================================" -ForegroundColor Green
Write-Host "源码: https://github.com/somthingthere/phanthywork2api" -ForegroundColor Cyan
if ($doRelease) {
    Write-Host "镜像: https://github.com/somthingthere/phanthywork2api/pkgs/container/phanthywork2api" -ForegroundColor Cyan
    Write-Host "Release: https://github.com/somthingthere/phanthywork2api/releases" -ForegroundColor Cyan
}
