[CmdletBinding()]
param(
    [string]$ProfileRoot = ".\services\api\data\doubao\profiles",
    [string]$AccountId = "",
    [string]$BrowserExecutable = "",
    [string]$EvidencePath = ""
)

$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent $PSScriptRoot
$sourceRoot = if ([IO.Path]::IsPathRooted($ProfileRoot)) {
    [IO.Path]::GetFullPath($ProfileRoot)
} else {
    [IO.Path]::GetFullPath((Join-Path $workspace $ProfileRoot))
}
if (-not (Test-Path -LiteralPath $sourceRoot -PathType Container)) {
    throw "Doubao profile root does not exist: $sourceRoot"
}

if ([string]::IsNullOrWhiteSpace($AccountId)) {
    $candidate = Get-ChildItem -LiteralPath $sourceRoot -Directory |
        Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName "browser") } |
        Sort-Object Name |
        Select-Object -First 1
    if ($null -eq $candidate) {
        throw "No Doubao browser profile was found under: $sourceRoot"
    }
    $AccountId = $candidate.Name
}

if ($AccountId -notmatch "^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$") {
    throw "AccountId contains unsupported path characters."
}

$sourceProfile = Join-Path (Join-Path $sourceRoot $AccountId) "browser"
if (-not (Test-Path -LiteralPath $sourceProfile -PathType Container)) {
    throw "Doubao browser profile does not exist: $sourceProfile"
}

if ([string]::IsNullOrWhiteSpace($BrowserExecutable)) {
    $candidates = @(
        "C:\Program Files\Google\Chrome\Application\chrome.exe",
        "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
    )
    $BrowserExecutable = $candidates |
        Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } |
        Select-Object -First 1
}
if ([string]::IsNullOrWhiteSpace($BrowserExecutable) -or -not (Test-Path -LiteralPath $BrowserExecutable -PathType Leaf)) {
    throw "A Chrome or Edge executable is required for browser E2E."
}

$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-browser-e2e-" + [guid]::NewGuid().ToString("N"))
$profileRoot = Join-Path $tempRoot "profiles"
$targetAccount = Join-Path $profileRoot $AccountId
$targetProfile = Join-Path $targetAccount "browser"
$binary = Join-Path $tempRoot "doubao-browser-worker.exe"
$stdout = Join-Path $tempRoot "stdout.log"
$stderr = Join-Path $tempRoot "stderr.log"
$port = Get-Random -Minimum 19400 -Maximum 19850
$token = "wbt_browser_e2e_token_20261009_long"
$process = $null
$sessionID = ""
$accountFingerprint = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($AccountId))).ToLowerInvariant()
$result = [ordered]@{
    format = "all2api-go-browser-e2e-v1"
    checked_at = [DateTime]::UtcNow.ToString("o")
    secrets_logged = $false
    account_id = "redacted"
    account_id_sha256 = $accountFingerprint
    browser = [IO.Path]::GetFileName($BrowserExecutable)
    status = "failed"
    worker_status = ""
    session_status = ""
    authenticated = $false
    page_url = ""
    page_title = ""
    cleanup = "pending"
}

function Write-Evidence {
    if (-not [string]::IsNullOrWhiteSpace($EvidencePath)) {
        $destination = [IO.Path]::GetFullPath($EvidencePath)
        New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
        $result | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $destination -Encoding UTF8
    }
    $result | ConvertTo-Json -Depth 8
}

try {
    New-Item -ItemType Directory -Path $targetAccount -Force | Out-Null
    Copy-Item -LiteralPath $sourceProfile -Destination $targetProfile -Recurse -Force
    & go build -mod=readonly -trimpath -o $binary (Join-Path $workspace "cmd/doubao-browser-worker")
    if ($LASTEXITCODE -ne 0) {
        throw "Go browser worker build failed"
    }

    $env:A2A_DOUBAO_BROWSER_ENABLED = "true"
    $env:A2A_DOUBAO_BROWSER_WORKER_TOKEN = $token
    $env:A2A_DOUBAO_BROWSER_WORKER_PORT = [string]$port
    $env:A2A_DOUBAO_PLATFORM_BASE = "https://www.doubao.com"
    $env:A2A_DOUBAO_PROFILE_ROOT = $profileRoot
    $env:A2A_DOUBAO_BROWSER_EXECUTABLE = $BrowserExecutable
    $env:A2A_DOUBAO_BROWSER_HEADLESS = "true"
    $env:A2A_DOUBAO_BROWSER_LAUNCH_TIMEOUT_SECONDS = "45"
    $env:A2A_DOUBAO_BROWSER_OPERATION_TIMEOUT_SECONDS = "45"

    $process = Start-Process -FilePath $binary -WorkingDirectory $workspace -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
    $health = $null
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if ($process.HasExited) {
            throw "browser worker exited before healthz"
        }
        try {
            $health = Invoke-RestMethod -Uri ("http://127.0.0.1:{0}/healthz" -f $port) -Headers @{ "X-Worker-Token" = $token } -TimeoutSec 2
            break
        } catch {
            Start-Sleep -Milliseconds 250
        }
    }
    if ($null -eq $health) {
        throw "browser worker healthz timed out"
    }
    $result.worker_status = [string]$health.status
    if ($health.status -ne "ready") {
        throw "browser worker is not ready"
    }

    $body = @{ account_id = $AccountId; profile_path = $targetProfile } | ConvertTo-Json -Compress
    $challenge = Invoke-RestMethod -Method Post -Uri ("http://127.0.0.1:{0}/v1/sessions" -f $port) -Headers @{ "X-Worker-Token" = $token } -ContentType "application/json" -Body $body -TimeoutSec 90
    $sessionID = [string]$challenge.session_id
    $result.session_status = [string]$challenge.status
    if ([string]::IsNullOrWhiteSpace($sessionID)) {
        throw "browser worker did not return a session id"
    }

    for ($attempt = 0; $attempt -lt 3; $attempt++) {
        Start-Sleep -Seconds 3
        $event = Invoke-RestMethod -Uri ("http://127.0.0.1:{0}/v1/sessions/{1}" -f $port, $sessionID) -Headers @{ "X-Worker-Token" = $token } -TimeoutSec 30
        $result.session_status = [string]$event.status
        $result.authenticated = [bool]($event.status -eq "succeeded")
        $result.page_url = [string]$event.url
        $result.page_title = [string]$event.title
        if ($result.authenticated) {
            break
        }
    }

    if (-not $result.authenticated) {
        throw "Doubao browser profile was not authenticated"
    }
    $result.status = "ok"
} catch {
    $message = $_.Exception.Message
    if ($message.Length -gt 200) {
        $message = $message.Substring(0, 200)
    }
    $result.error = $message.Replace([Environment]::NewLine, " ")
} finally {
    if ($sessionID) {
        try {
            $null = Invoke-WebRequest -Method Delete -Uri ("http://127.0.0.1:{0}/v1/sessions/{1}" -f $port, $sessionID) -Headers @{ "X-Worker-Token" = $token } -TimeoutSec 5
        } catch {
        }
    }
    if ($null -ne $process) {
        if (-not $process.HasExited) {
            $process.Kill()
        }
        $process.WaitForExit()
        $process.Dispose()
    }
    $result.cleanup = "worker_stopped"
    try {
        if (Test-Path -LiteralPath $tempRoot) {
            Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction Stop
        }
    } catch {
        $result.cleanup = "worker_stopped_temp_cleanup_failed"
    }
    Write-Evidence
}

if ($result.status -ne "ok") {
    exit 2
}
