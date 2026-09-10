#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
run.ps1 — 在本机直接运行 workbuddy2api（不依赖 Docker）

用法:
  .\run.ps1                    # 使用 config.json
  .\run.ps1 -Config other.json # 指定其它配置

行为:
  1. 切到脚本所在目录（config.json / auths / data 均为相对路径，必须在项目根目录运行）
  2. wb2api.exe 不存在时自动 go build
  3. 前台运行，按 Ctrl+C 停止
  4. 启动失败时自动诊断端口问题（系统保留段 / 被占用）

想在后台常驻，用：
  Start-Process -FilePath .\wb2api.exe -WindowStyle Hidden
  停止：Get-Process wb2api | Stop-Process

端口由 config.json 的 listen 决定（默认 127.0.0.1:8787）。
#>
[CmdletBinding()]
param(
    [string]$Config = 'config.json'
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
Set-Location -LiteralPath $Root

$Exe = Join-Path $Root 'wb2api.exe'
if (-not (Test-Path -LiteralPath $Exe)) {
    Write-Host 'build wb2api.exe ...'
    go build -o $Exe ./cmd/server
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

& $Exe -config $Config
$code = $LASTEXITCODE

if ($code -ne 0) {
    try {
        $cfgPath = $Config
        if (-not [System.IO.Path]::IsPathRooted($cfgPath)) { $cfgPath = Join-Path $Root $Config }
        $listen = [string](Get-Content -LiteralPath $cfgPath -Raw | ConvertFrom-Json).listen

        $port = 0
        if ($listen -match ':(\d+)\s*$') { $port = [int]$Matches[1] }

        if ($port -gt 0) {
            Write-Host ''
            Write-Host "启动失败。端口 $port 排查："

            $busy = $false
            foreach ($l in @(Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue)) {
                $busy = $true
                $pname = '?'
                try { $pname = (Get-Process -Id $l.OwningProcess -ErrorAction Stop).ProcessName } catch { }
                Write-Host "  [占用] $($l.LocalAddress):$port  pid=$($l.OwningProcess)  $pname"
            }
            if ($busy) {
                Write-Host '         若是 wb2api 自身：Get-Process wb2api | Stop-Process'
            }

            foreach ($line in @(netsh int ipv4 show excludedportrange protocol=tcp 2>$null)) {
                if ($line -match '^\s*(\d+)\s+(\d+)') {
                    $lo = [int]$Matches[1]
                    $hi = [int]$Matches[2]
                    if ($port -ge $lo -and $port -le $hi) {
                        Write-Host "  [保留段] $port 落在系统预留的 $lo-$hi 内（Hyper-V / WSL2 / Docker Desktop 所致）"
                        Write-Host '           这类端口任何程序都绑不上，换一个即可（重启后保留段会漂移）。'
                        Write-Host '           查全部保留段：netsh int ipv4 show excludedportrange protocol=tcp'
                    }
                }
            }

            Write-Host '  建议：编辑 config.json 的 "listen" 换个端口（或设环境变量 WB2A_LISTEN），'
            Write-Host '        例如 127.0.0.1:8787 / 127.0.0.1:9000 / 127.0.0.1:28080。'
        }
    } catch {
        Write-Host ''
        Write-Host "（端口诊断未能完成：$($_.Exception.Message)）"
    }
}

exit $code
