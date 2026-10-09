[CmdletBinding()]
param(
    [string]$PreviousBinary = "",
    [string]$RealE2EReport = "",
    [string]$BrowserE2EReport = "",
    [string]$ReportPath = "",
    [string]$DeploymentEnvironment = ""
)

$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent $PSScriptRoot
$checks = [ordered]@{}
$blocking = [System.Collections.Generic.List[string]]::new()

function Add-Check {
    param([string]$Name, [bool]$Passed, [string]$Detail)
    $script:checks[$Name] = [ordered]@{ passed = $Passed; detail = $Detail }
    if (-not $Passed) { $script:blocking.Add($Name + ": " + $Detail) }
}

function Detail {
    param([bool]$Passed, [string]$Success, [string]$Failure)
    if ($Passed) { return $Success }
    return $Failure
}

$dockerInfo = @()
$dockerAvailable = $false
try {
    $dockerInfo = @(docker info --format '{{.ServerVersion}}' 2>$null)
    $dockerAvailable = $LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace(($dockerInfo -join ""))
} catch {
    $dockerAvailable = $false
}
Add-Check "docker_daemon" $dockerAvailable (Detail $dockerAvailable "server=$($dockerInfo -join '')" "Docker daemon is unavailable")

$composeOutput = @()
$composeValid = $false
try {
    $composeOutput = @(docker compose -f (Join-Path $workspace "docker-compose.go.yml") config --quiet 2>&1)
    $composeValid = $LASTEXITCODE -eq 0
} catch {
    $composeValid = $false
}
Add-Check "go_compose_config" $composeValid (Detail $composeValid "valid" ($composeOutput -join " "))

$workflowPath = Join-Path $workspace ".github/workflows/go-migration.yml"
$workflowText = if (Test-Path -LiteralPath $workflowPath -PathType Leaf) { Get-Content -LiteralPath $workflowPath -Raw -Encoding UTF8 } else { "" }
$workflowPassed = $workflowText.Contains("go-image-build") -and $workflowText.Contains("docker build --file Dockerfile.golang") -and $workflowText.Contains("go-release-artifacts") -and $workflowText.Contains("generate-go-sbom.ps1") -and $workflowText.Contains("go-vulnerability-scan") -and $workflowText.Contains("govulncheck@v1.1.4") -and $workflowText.Contains("go-license-scan") -and $workflowText.Contains("go-licenses@v1.3.0") -and $workflowText.Contains("go-supply-chain-local") -and $workflowText.Contains("verify-go-supply-chain.py")
Add-Check "ci_release_gates" $workflowPassed (Detail $workflowPassed "Docker, artifact/SBOM, vulnerability, license and local supply-chain jobs are present" "CI must include Dockerfile.golang, release/SBOM, govulncheck, go-licenses and local module audit gates")

$supplyChainPath = Join-Path $workspace "docs/migration-evidence/supply-chain-2026-10-09.json"
$supplyChainPassed = $false
$supplyChainDetail = "supply-chain evidence is missing"
if (Test-Path -LiteralPath $supplyChainPath -PathType Leaf) {
    try {
        $supplyChain = Get-Content -LiteralPath $supplyChainPath -Raw -Encoding UTF8 | ConvertFrom-Json
        $supplyChainPassed = $supplyChain.go_mod_verify.status -eq "ok" -and $supplyChain.govulncheck.status -eq "ok" -and $supplyChain.go_licenses.status -eq "ok"
        $supplyChainDetail = if ($supplyChainPassed) { "module, vulnerability and dependency-license evidence passed" } else { "module integrity passed but vulnerability/license evidence is unverified" }
    } catch {
        $supplyChainDetail = "supply-chain evidence is invalid JSON"
    }
}
Add-Check "supply_chain_verification" $supplyChainPassed $supplyChainDetail

$rootLicense = @("LICENSE", "LICENSE.md", "COPYING") | Where-Object { Test-Path -LiteralPath (Join-Path $workspace $_) -PathType Leaf }
$licensePassed = @($rootLicense).Count -gt 0
Add-Check "root_license_declared" $licensePassed (Detail $licensePassed ("declared: " + ($rootLicense -join ", ")) "root LICENSE/COPYING is missing; public release requires an explicit licensing decision")

$environment = $DeploymentEnvironment
if ([string]::IsNullOrWhiteSpace($environment)) { $environment = $env:A2A_ENV }
$environmentPassed = $environment -eq "production"
Add-Check "production_environment" $environmentPassed (Detail $environmentPassed "A2A_ENV=production" "cutover requires -DeploymentEnvironment production or A2A_ENV=production")

$defaultCompose = Get-Content -LiteralPath (Join-Path $workspace "docker-compose.yml") -Raw -Encoding UTF8
$defaultHasPython = $defaultCompose -match "python|uvicorn|services/api"
Add-Check "default_compose_is_go" (-not $defaultHasPython) (Detail (-not $defaultHasPython) "Go-only" "default docker-compose.yml still contains Python runtime")

$pythonRuntime = Test-Path -LiteralPath (Join-Path $workspace "services/api/pyproject.toml")
Add-Check "python_runtime_removed" (-not $pythonRuntime) (Detail (-not $pythonRuntime) "removed" "services/api/pyproject.toml still exists")

$legacyBridge = Test-Path -LiteralPath (Join-Path $workspace "services/api/app/compat")
Add-Check "legacy_bridge_removed" (-not $legacyBridge) (Detail (-not $legacyBridge) "removed" "services/api/app/compat still exists")

$backupScripts = (Test-Path -LiteralPath (Join-Path $workspace "scripts/backup-go-state.ps1")) -and (Test-Path -LiteralPath (Join-Path $workspace "scripts/verify-go-backup-restore.ps1"))
Add-Check "backup_restore_scripts" $backupScripts (Detail $backupScripts "present" "backup/restore scripts are missing")

$previous = $false
if (-not [string]::IsNullOrWhiteSpace($PreviousBinary)) { $previous = Test-Path -LiteralPath ([IO.Path]::GetFullPath($PreviousBinary)) }
$previousDetail = "an archived previous Go binary was not supplied"
if ($previous) { $previousDetail = [IO.Path]::GetFullPath($PreviousBinary) }
Add-Check "previous_go_binary" $previous $previousDetail

function Read-E2EReport {
    param([string]$Path, [string]$Name)
    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        Add-Check $Name $false "a JSON evidence report was not supplied"
        return
    }
    try { $value = Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json } catch { Add-Check $Name $false "report is not valid JSON"; return }
    $items = @($value.channels)
    $passed = $items.Count -eq 3 -and (@($items | Where-Object { $_.status -ne "ok" }).Count -eq 0)
    if ($passed) {
        Add-Check $Name $true "three channels reported ok"
        return
    }
    $failures = @($items | Where-Object { $_.status -ne "ok" } | ForEach-Object {
        $code = if ($_.error_code) { [string]$_.error_code } else { "unknown" }
        "{0}:{1}" -f ([string]$_.channel), $code
    })
    $detail = if ($failures.Count -gt 0) { "failed channels: " + ($failures -join ", ") } else { "report must contain three channels with status=ok" }
    Add-Check $Name $false $detail
}

function Read-BrowserReport {
    param([string]$Path, [string]$Name)
    if ([string]::IsNullOrWhiteSpace($Path) -or -not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        Add-Check $Name $false "a browser E2E evidence report was not supplied"
        return
    }
    try { $value = Get-Content -LiteralPath $Path -Raw -Encoding UTF8 | ConvertFrom-Json } catch { Add-Check $Name $false "report is not valid JSON"; return }
    $channels = @($value.channels)
    if ($channels.Count -eq 0 -or $null -eq $channels[0]) { $channels = @($value.profiles) }
    $passed = $value.status -eq "ok"
    if (-not $passed -and $channels.Count -eq 3) {
        $passed = (@($channels | Where-Object { $_.status -ne "ok" }).Count -eq 0)
    }
    if ($passed) {
        Add-Check $Name $true "browser E2E reported ok"
        return
    }
    $failures = @()
    foreach ($profile in $channels) {
        if ([string]$profile.status -eq "ok") { continue }
        $reason = [string]$profile.error
        if ([string]::IsNullOrWhiteSpace($reason) -and $profile.authenticated -eq $false) { $reason = "authenticated=false" }
        if ([string]::IsNullOrWhiteSpace($reason)) { $reason = "unknown" }
        $failures += ("{0}:{1}" -f ([string]$profile.account_id_sha256), $reason)
    }
    $detail = if ($failures.Count -gt 0) { "failed profiles: " + ($failures -join ", ") } else { "browser E2E report must report status=ok" }
    Add-Check $Name $false $detail
}

Read-E2EReport -Path $RealE2EReport -Name "real_platform_e2e"
Read-BrowserReport -Path $BrowserE2EReport -Name "browser_e2e"

$result = [ordered]@{
    format = "all2api-go-cutover-readiness-v1"
    checked_at = [DateTime]::UtcNow.ToString("o")
    ready = ($blocking.Count -eq 0)
    checks = $checks
    blockers = @($blocking)
}
if (-not [string]::IsNullOrWhiteSpace($ReportPath)) {
    $destination = [IO.Path]::GetFullPath($ReportPath)
    New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
    $result | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $destination -Encoding UTF8
}
$result | ConvertTo-Json -Depth 8
if (-not $result.ready) { exit 2 }
