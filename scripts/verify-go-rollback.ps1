[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$BackupRoot,

    [Parameter(Mandatory = $true)]
    [string]$RollbackBinary,

    [switch]$KeepTemp
)

$ErrorActionPreference = "Stop"
$backup = [IO.Path]::GetFullPath($BackupRoot)
$binary = [IO.Path]::GetFullPath($RollbackBinary)
if (-not (Test-Path -LiteralPath (Join-Path $backup "all2api.db") -PathType Leaf)) {
    throw "Backup database is missing: $backup"
}
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    throw "Rollback binary is missing: $binary"
}

$restore = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-rollback-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $restore | Out-Null
$process = $null
$port = Get-Random -Minimum 19100 -Maximum 19900
$old = @{}
foreach ($name in @("A2A_HOST", "A2A_PORT", "A2A_DB_PATH", "A2A_STATE_PATH", "A2A_SESSION_SECRET", "A2A_ADMIN_USERNAME", "A2A_ADMIN_PASSWORD", "A2A_ADMIN_TOKEN")) {
    $old[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
}

try {
    Copy-Item -LiteralPath (Join-Path $backup "all2api.db") -Destination (Join-Path $restore "all2api.db")
    foreach ($suffix in @("-wal", "-shm")) {
        $sidecar = Join-Path $backup ("all2api.db" + $suffix)
        if (Test-Path -LiteralPath $sidecar -PathType Leaf) {
            Copy-Item -LiteralPath $sidecar -Destination (Join-Path $restore ("all2api.db" + $suffix))
        }
    }
    $mediaSource = Join-Path $backup "media-assets"
    if (Test-Path -LiteralPath $mediaSource -PathType Container) {
        Copy-Item -LiteralPath $mediaSource -Destination (Join-Path $restore "media-assets") -Recurse -Force
    }

    $env:A2A_HOST = "127.0.0.1"
    $env:A2A_PORT = [string]$port
    $env:A2A_DB_PATH = Join-Path $restore "all2api.db"
    $env:A2A_STATE_PATH = Join-Path $restore "state.json"
    $env:A2A_SESSION_SECRET = "rollback-verification-session-secret-012345"
    $env:A2A_ADMIN_USERNAME = "admin"
    $env:A2A_ADMIN_PASSWORD = "rollback-verification-password"
    $env:A2A_ADMIN_TOKEN = "wbt_rollback_verification_token_20261008_long"
    $stdout = Join-Path $restore "stdout.log"
    $stderr = Join-Path $restore "stderr.log"
    $process = Start-Process -FilePath $binary -WorkingDirectory (Split-Path -Parent $PSScriptRoot) -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
    $health = $null
    for ($attempt = 0; $attempt -lt 80; $attempt++) {
        if ($process.HasExited) { throw "Rollback binary exited with code $($process.ExitCode)" }
        try {
            $raw = & curl.exe --noproxy "*" -sS --max-time 1 "http://127.0.0.1:$port/admin/api/healthz?detail=true" 2>$null
            if ($LASTEXITCODE -eq 0) { $health = ($raw -join "`n") | ConvertFrom-Json; break }
        } catch { $health = $null }
        Start-Sleep -Milliseconds 250
    }
    if ($null -eq $health -or $health.status -ne "ok" -or $health.database -ne "ok" -or [int]$health.checks.storage.schema_version -ne 11) {
        throw "Rollback binary did not read the backup successfully"
    }
    Write-Output ("Go rollback rehearsal passed: schema={0}, binary={1}" -f $health.checks.storage.schema_version, [IO.Path]::GetFileName($binary))
} finally {
    if ($null -ne $process) {
        if (-not $process.HasExited) { $process.Kill() }
        $process.WaitForExit()
        $process.Dispose()
    }
    foreach ($name in $old.Keys) { [Environment]::SetEnvironmentVariable($name, $old[$name], "Process") }
    if (-not $KeepTemp -and (Test-Path -LiteralPath $restore)) { Remove-Item -LiteralPath $restore -Recurse -Force }
}
