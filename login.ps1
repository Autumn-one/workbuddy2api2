#!/usr/bin/env pwsh
#Requires -Version 5.1
<#
login.ps1 — WorkBuddy CN OAuth 登录 → 落盘 auth 文件 —— login.sh 的 PowerShell 等价版

用法:
  .\login.ps1

流程:
  1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
  2. 你在浏览器打开 URL 完成登录
  3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auths\workbuddy-<uid>.json
  4. 重启 workbuddy2api 容器加载新账号

二进制升级: go build -o login.exe .\cmd\login
#>
[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

$Root      = $PSScriptRoot
$AuthDir   = Join-Path $Root 'auths'
$Container = 'workbuddy2api'

Set-Location -LiteralPath $Root
New-Item -ItemType Directory -Force -Path $AuthDir | Out-Null

# login 工具：不存在才编译（源码改动后手动 go build -o login.exe .\cmd\login）
$LoginBin = Join-Path $Root 'login.exe'
if (-not (Test-Path -LiteralPath $LoginBin)) {
    go build -o $LoginBin ./cmd/login
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Write-Host '============================================================'
Write-Host '  WorkBuddy OAuth 登录'
Write-Host '============================================================'
Write-Host ''

$AuthUrl = & $LoginBin url
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host '请在浏览器中打开以下链接完成登录：'
Write-Host ''
Write-Host "  $AuthUrl"
Write-Host ''

# 剪贴板为尽力而为：对应 sh 里 xclip/xsel 的可选分支，失败不中断
try {
    Set-Clipboard -Value $AuthUrl
    Write-Host '(已复制到剪贴板)'
} catch {
}

Write-Host ''
$ans = Read-Host '完成登录后按 y 继续'
if ($ans -cne 'y' -and $ans -cne 'Y') {
    Write-Host '已取消'
    exit 1
}

Write-Host ''
Write-Host '正在获取 token...'

$Result = & $LoginBin poll
if ($LASTEXITCODE -ne 0) {
    Write-Host ''
    Write-Host '获取 token 失败。可能原因：'
    Write-Host '  - 登录还没完成就按了 y（重新运行 .\login.ps1 再试）'
    Write-Host '  - 登录页报错（把报错截图发出来排查）'
    exit 1
}

$j = ($Result | Out-String).Trim() | ConvertFrom-Json

# 缺字段一律归一为空串，等价于 sh 里 python 的 .get(key, '')
$Token     = [string]$j.access_token
$Refresh   = [string]$j.refresh_token
$ExpiresIn = [int64]$j.expires_in
$Domain    = [string]$j.domain
$UserId    = [string]$j.uid
$EntId     = [string]$j.enterprise_id
$Nickname  = [string]$j.nickname

if ([string]::IsNullOrEmpty($UserId)) {
    Write-Host '无法获取 uid，请检查 token 是否有效'
    exit 1
}

$ExpiresAt = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() + $ExpiresIn

# ─── 提取错误响应的 body（PS 7 走 ErrorDetails/Content，Windows PowerShell 5.1 走 Response 流）───
function Get-ErrorBodyText($Err) {
    if ($Err.ErrorDetails -and $Err.ErrorDetails.Message) {
        return $Err.ErrorDetails.Message
    }
    $resp = $Err.Exception.Response
    if ($resp) {
        try {
            if ($resp.PSObject.Properties['Content']) {
                $t = $resp.Content.ReadAsStringAsync().GetAwaiter().GetResult()
                if ($t) { return $t }
            }
        } catch { }
        try {
            $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
            $t = $reader.ReadToEnd()
            $reader.Dispose()
            if ($t) { return $t }
        } catch { }
    }
    return $null
}

# ─── 签到（CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，幂等不阻塞）───
$Headers = @{
    Authorization = "Bearer $Token"
    Accept        = 'application/json'
    'X-User-Id'   = $UserId
}
if ($EntId) {
    $Headers['X-Enterprise-Id'] = $EntId
    $Headers['X-Tenant-Id']     = $EntId
}
if ($Domain) {
    $Headers['X-Domain'] = $Domain
}

try {
    $resp = Invoke-RestMethod -Method Post -Uri 'https://www.codebuddy.cn/v2/billing/meter/daily-checkin' `
        -Headers $Headers -ContentType 'application/json' -Body '{}' -TimeoutSec 15

    if ($resp.code -eq 0) {
        if ($null -eq $resp.data) {
            $data = '{}'
        } else {
            $data = $resp.data | ConvertTo-Json -Depth 10 -Compress
        }
        Write-Host ('签到: 成功 ' + $data.Substring(0, [Math]::Min(150, $data.Length)))
    } else {
        if ($resp.msg) {
            $m = [string]$resp.msg
        } else {
            $m = $resp | ConvertTo-Json -Depth 10 -Compress
        }
        Write-Host ('签到: ' + $m.Substring(0, [Math]::Min(150, $m.Length)))
    }
} catch {
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    $body = Get-ErrorBodyText $_
    $parsed = $null
    if ($body) { try { $parsed = $body | ConvertFrom-Json } catch { } }
    if ($parsed -and $parsed.msg) {
        Write-Host "签到: $($parsed.msg)"
    } elseif ($body) {
        Write-Host "签到: $body"
    } else {
        Write-Host "签到: $($_.Exception.Message)"
    }
}

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
$AuthFile = Join-Path $AuthDir "workbuddy-$UserId.json"
if (Test-Path -LiteralPath $AuthFile) {
    Write-Host "账号已存在（uid=$UserId），将覆盖更新凭证"
    $Action = '覆盖'
} else {
    Write-Host "新账号（uid=$UserId），新增 auth 文件"
    $Action = '新增'
}

$auth = [ordered]@{
    account = [ordered]@{
        uid          = $UserId
        enterpriseId = $EntId
        nickname     = $Nickname
    }
    auth    = [ordered]@{
        accessToken  = $Token
        refreshToken = $Refresh
        expiresAt    = $ExpiresAt
        domain       = $Domain
    }
}

$json = $auth | ConvertTo-Json -Depth 10
# File.WriteAllText 默认 UTF-8 无 BOM，且用绝对路径——绕开 .NET 相对路径不跟随 Set-Location 的坑
[System.IO.File]::WriteAllText($AuthFile, $json)
Write-Host "已保存（$Action）: $AuthFile"

# ─── 重启服务 ────────────────────────────────────────────
Write-Host ''
$names = docker ps --format '{{.Names}}'
if ($names -contains $Container) {
    Write-Host "重启 $Container 加载新账号..."
    docker restart $Container | Out-Null
    Start-Sleep -Seconds 2

    # API_KEY 从 config.json 读取（读不到才回落硬编码值，避免 401）
    $ApiKey = ''
    $ConfigPath = Join-Path $Root 'config.json'
    if (Test-Path -LiteralPath $ConfigPath) {
        try {
            $ApiKey = [string](Get-Content -LiteralPath $ConfigPath -Raw | ConvertFrom-Json).api_key
        } catch { }
    }
    if ([string]::IsNullOrEmpty($ApiKey)) { $ApiKey = 'tistzach' }

    $Count = '?'
    try {
        $st = Invoke-RestMethod -Uri 'http://127.0.0.1:7863/status' `
            -Headers @{ Authorization = "Bearer $ApiKey" } -TimeoutSec 15
        $Count = @($st.accounts).Count
    } catch { }
    Write-Host "服务已重启，当前账号数: $Count"
} else {
    # 没跑容器，但可能是本机直接 run.ps1 启动的 wb2api.exe（它只在启动时读一次 auths\）
    $native = @(Get-Process -Name 'wb2api' -ErrorAction SilentlyContinue)
    if ($native.Count -gt 0) {
        Write-Host "检测到本机 wb2api.exe 正在运行（PID $($native[0].Id)），但账号只在启动时加载一次。"
        Write-Host "请重启它让新账号生效：  Get-Process wb2api | Stop-Process   然后   .\run.ps1"
    } else {
        Write-Host "容器 $Container 未运行，auth 文件已保存，下次启动自动加载"
    }
}

Write-Host ''
Write-Host '============================================================'
Write-Host '  登录完成！'
Write-Host "  UID: $UserId"
if ($Nickname) {
    Write-Host "  Nickname: $Nickname"
} else {
    Write-Host '  Nickname: （未获取到）'
}
Write-Host ('  Token: ' + $Token.Substring(0, [Math]::Min(30, $Token.Length)) + '...')
$ExpiresText = [DateTimeOffset]::FromUnixTimeSeconds($ExpiresAt).LocalDateTime.ToString('yyyy-MM-dd HH:mm')
Write-Host "  有效期: $ExpiresText"
Write-Host '============================================================'
