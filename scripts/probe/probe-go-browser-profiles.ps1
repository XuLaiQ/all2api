[CmdletBinding()]
param(
    [string]$ProfileRoot = ".\services\api\data\doubao\profiles",
    [string]$BrowserExecutable = "",
    [string]$EvidencePath = ""
)

$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent $PSScriptRoot
$scriptPath = Join-Path $PSScriptRoot "probe-go-browser.ps1"
$sourceRoot = if ([IO.Path]::IsPathRooted($ProfileRoot)) {
    [IO.Path]::GetFullPath($ProfileRoot)
} else {
    [IO.Path]::GetFullPath((Join-Path $workspace $ProfileRoot))
}
if (-not (Test-Path -LiteralPath $sourceRoot -PathType Container)) {
    throw "Doubao profile root does not exist: $sourceRoot"
}

$accounts = @(Get-ChildItem -LiteralPath $sourceRoot -Directory |
    Where-Object {
        $_.Name -match "^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$" -and
        (Test-Path -LiteralPath (Join-Path $_.FullName "browser") -PathType Container)
    } |
    Sort-Object Name |
    Select-Object -ExpandProperty Name)
if ($accounts.Count -eq 0) {
    throw "No usable Doubao browser profiles were found under: $sourceRoot"
}

$aggregate = [ordered]@{
    format = "all2api-go-browser-profiles-e2e-v1"
    checked_at = [DateTime]::UtcNow.ToString("o")
    secrets_logged = $false
    profile_count = $accounts.Count
    profiles = @()
    status = "failed"
}
$attemptRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-browser-profiles-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $attemptRoot | Out-Null

try {
    foreach ($account in $accounts) {
        $attemptPath = Join-Path $attemptRoot ($account + ".json")
        $arguments = @(
            "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $scriptPath,
            "-ProfileRoot", $sourceRoot,
            "-AccountId", $account,
            "-EvidencePath", $attemptPath
        )
        if (-not [string]::IsNullOrWhiteSpace($BrowserExecutable)) {
            $arguments += @("-BrowserExecutable", $BrowserExecutable)
        }
        & pwsh @arguments *> $null
        if (Test-Path -LiteralPath $attemptPath -PathType Leaf) {
            $attempt = Get-Content -LiteralPath $attemptPath -Raw -Encoding UTF8 | ConvertFrom-Json
            $aggregate.profiles += [ordered]@{
                account_id = "redacted"
                account_id_sha256 = [string]$attempt.account_id_sha256
                browser = [string]$attempt.browser
                worker_status = [string]$attempt.worker_status
                session_status = [string]$attempt.session_status
                authenticated = [bool]$attempt.authenticated
                page_url = [string]$attempt.page_url
                page_title = [string]$attempt.page_title
                status = [string]$attempt.status
                error = if ($attempt.error) { [string]$attempt.error } else { "" }
            }
        } else {
            $aggregate.profiles += [ordered]@{
                account_id = "redacted"
                status = "failed"
                error = "profile probe did not produce evidence"
            }
        }
    }
    if (@($aggregate.profiles | Where-Object { $_.status -eq "ok" }).Count -gt 0) {
        $aggregate.status = "ok"
    }
} finally {
    if (-not [string]::IsNullOrWhiteSpace($EvidencePath)) {
        $destination = [IO.Path]::GetFullPath($EvidencePath)
        New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
        $aggregate | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $destination -Encoding UTF8
    }
    if (Test-Path -LiteralPath $attemptRoot) {
        Remove-Item -LiteralPath $attemptRoot -Recurse -Force
    }
    $aggregate | ConvertTo-Json -Depth 8
}

if ($aggregate.status -ne "ok") {
    exit 2
}
