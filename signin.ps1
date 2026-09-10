#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
signin.ps1 — 批量签到：遍历 auths\ 下所有 workbuddy-*.json 账号 —— signin.sh 的 PowerShell 等价版

用法:
  .\signin.ps1                 # 默认 auths 目录
  .\signin.ps1 -AuthsDir other\auths

二进制升级: go build -o signin_bin.exe .\cmd\signin
#>
[CmdletBinding()]
param(
    [string]$AuthsDir = 'auths'
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
Set-Location -LiteralPath $Root

$Exe = Join-Path $Root 'signin_bin.exe'
if (-not (Test-Path -LiteralPath $Exe)) {
    Write-Host 'build signin_bin ...'
    go build -o $Exe ./cmd/signin
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

& $Exe $AuthsDir

exit $LASTEXITCODE
