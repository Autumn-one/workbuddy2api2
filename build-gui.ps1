#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
build-gui.ps1 — 构建桌面控制台 wb2api-gui.exe

注意：GUI 依赖 comctl32 v6 清单（cmd/gui/app.manifest）。
缺少它 walk 会以 "TTM_ADDTOOL failed" 直接启动失败，不只是外观问题。
清单通过 cmd/gui/rsrc_windows_amd64.syso 嵌入 exe（Go 链接器自动识别同目录 .syso）。

rsrc 安装一次即可：go install github.com/akavel/rsrc@latest
若清单有改动，本脚本会自动重新生成 syso。
#>
[CmdletBinding()]
param(
    [switch]$Console   # 构建带控制台的调试版（可直接看到 panic 堆栈）
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
Set-Location -LiteralPath $Root

if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }

$syso = Join-Path $Root 'cmd\gui\rsrc_windows_amd64.syso'
$rsrc = Join-Path (go env GOPATH) 'bin\rsrc.exe'

if (Test-Path -LiteralPath $rsrc) {
    & $rsrc -arch amd64 -manifest (Join-Path $Root 'cmd\gui\app.manifest') -o $syso
    if ($LASTEXITCODE -ne 0) { Write-Error 'rsrc 生成 syso 失败' }
} elseif (-not (Test-Path -LiteralPath $syso)) {
    Write-Warning '未找到 rsrc，且缺少 rsrc_windows_amd64.syso。'
    Write-Warning '请先执行: go install github.com/akavel/rsrc@latest'
    Write-Warning '否则产物会因缺少 comctl32 v6 清单而启动失败。'
}

$env:CGO_ENABLED = '0'
if ($Console) {
    go build -o wb2api-gui-console.exe ./cmd/gui
    Write-Host '已生成 wb2api-gui-console.exe（带控制台，便于排查）'
} else {
    go build -trimpath -ldflags="-s -w -H windowsgui" -o wb2api-gui.exe ./cmd/gui
    Write-Host '已生成 wb2api-gui.exe'
}
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
