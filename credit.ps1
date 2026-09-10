#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
credit.ps1 — WorkBuddy 积分日报（默认美化输出）—— credit.sh 的 PowerShell 等价版

用法:
  .\credit.ps1           # 人类可读日报
  .\credit.ps1 -Json     # 原始 JSON

二进制升级: go build -o credit.exe .\cmd\credit
#>
[CmdletBinding()]
param(
    [switch]$Json
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
Set-Location -LiteralPath $Root

$Exe = Join-Path $Root 'credit.exe'
if (-not (Test-Path -LiteralPath $Exe)) {
    go build -o $Exe ./cmd/credit
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

if ($Json) {
    & $Exe
} else {
    & $Exe -pretty
}

exit $LASTEXITCODE
