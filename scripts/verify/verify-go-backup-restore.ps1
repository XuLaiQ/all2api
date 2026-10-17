[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$BackupRoot,

    [switch]$KeepTemp
)

$ErrorActionPreference = "Stop"
$backup = [IO.Path]::GetFullPath($BackupRoot)
$manifestPath = Join-Path $backup "manifest.json"
if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    throw "Backup manifest does not exist: $manifestPath"
}

$manifest = Get-Content -LiteralPath $manifestPath -Raw -Encoding UTF8 | ConvertFrom-Json
if ([int]$manifest.format -ne 1) {
    throw "Unsupported backup manifest format: $($manifest.format)"
}

$restore = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-restore-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $restore | Out-Null
$process = $null
$oldEnvironment = @{}
$environmentNames = @(
    "A2A_HOST", "A2A_PORT", "A2A_DB_PATH", "A2A_STATE_PATH", "A2A_SESSION_SECRET",
    "A2A_CREDENTIAL_MASTER_KEY", "A2A_ADMIN_USERNAME", "A2A_ADMIN_PASSWORD", "A2A_ADMIN_TOKEN"
)

function Resolve-BoundedPath {
    param(
        [Parameter(Mandatory = $true)] [string]$Root,
        [Parameter(Mandatory = $true)] [string]$RelativePath
    )
    $rootPath = [IO.Path]::GetFullPath($Root)
    $candidate = [IO.Path]::GetFullPath((Join-Path $rootPath $RelativePath))
    $prefix = $rootPath.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    if ($candidate -ne $rootPath -and -not $candidate.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Backup manifest path escapes its root."
    }
    return $candidate
}

function Restore-Entry {
    param(
        [Parameter(Mandatory = $true)] [object]$Entry,
        [Parameter(Mandatory = $true)] [string]$DestinationRoot
    )
    $source = Resolve-BoundedPath -Root $backup -RelativePath ([string]$Entry.path)
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "Backup file is missing: $($Entry.path)"
    }
    $sourceHash = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($sourceHash -ne ([string]$Entry.sha256).ToLowerInvariant()) {
        throw "Backup hash mismatch: $($Entry.path)"
    }
    $destination = Resolve-BoundedPath -Root $DestinationRoot -RelativePath ([string]$Entry.path)
    $parent = Split-Path -Parent $destination
    New-Item -ItemType Directory -Path $parent -Force | Out-Null
    Copy-Item -LiteralPath $source -Destination $destination -Force
    $restoredHash = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($restoredHash -ne $sourceHash) {
        throw "Restored hash mismatch: $($Entry.path)"
    }
}

try {
    foreach ($entry in @($manifest.database) + @($manifest.media)) {
        Restore-Entry -Entry $entry -DestinationRoot $restore
    }

    $workspace = Split-Path -Parent $PSScriptRoot
    $binary = Join-Path $restore "all2api.exe"
    & go build -mod=readonly -trimpath -o $binary (Join-Path $workspace "cmd\all2api")
    if ($LASTEXITCODE -ne 0) {
        throw "Go restore verification build failed with exit code $LASTEXITCODE"
    }

    $port = Get-Random -Minimum 19000 -Maximum 19900
    foreach ($name in $environmentNames) {
        $oldEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
    }
    $env:A2A_HOST = "127.0.0.1"
    $env:A2A_PORT = [string]$port
    $env:A2A_DB_PATH = Join-Path $restore "all2api.db"
    $env:A2A_STATE_PATH = Join-Path $restore "state.json"
    $env:A2A_SESSION_SECRET = "restore-verification-session-secret-012345"
    $env:A2A_ADMIN_USERNAME = "admin"
    $env:A2A_ADMIN_PASSWORD = "restore-verification-password"
    $env:A2A_ADMIN_TOKEN = "wbt_restore_verification_token_20261008_0001"
    $stdout = Join-Path $restore "stdout.log"
    $stderr = Join-Path $restore "stderr.log"
    $process = Start-Process -FilePath $binary -WorkingDirectory $workspace -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru

    $health = $null
    for ($attempt = 0; $attempt -lt 80; $attempt++) {
        if ($process.HasExited) {
            throw "Go restore verification process exited with code $($process.ExitCode)"
        }
        try {
            $raw = & curl.exe --noproxy "*" -sS --max-time 1 "http://127.0.0.1:$port/admin/api/healthz?detail=true" 2>$null
            if ($LASTEXITCODE -eq 0) {
                $health = ($raw -join "`n") | ConvertFrom-Json
                break
            }
        } catch {
            $health = $null
        }
        Start-Sleep -Milliseconds 250
    }
    if ($null -eq $health) {
        throw "Go restore verification health check timed out"
    }
    if ($health.status -ne "ok" -or $health.database -ne "ok" -or [int]$health.checks.storage.schema_version -ne 11) {
        throw "Go restore verification returned an invalid health result"
    }
    Write-Output ("Go backup restore verification passed: schema={0}, media_files={1}" -f $health.checks.storage.schema_version, @($manifest.media).Count)
} finally {
    if ($null -ne $process) {
        if (-not $process.HasExited) {
            $process.Kill()
        }
        $process.WaitForExit()
        $process.Dispose()
    }
    foreach ($name in $environmentNames) {
        [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], "Process")
    }
    if (-not $KeepTemp -and (Test-Path -LiteralPath $restore)) {
        Remove-Item -LiteralPath $restore -Recurse -Force
    }
}
