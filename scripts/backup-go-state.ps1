[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$DatabasePath,

    [Parameter(Mandatory = $true)]
    [string]$MediaRoot,

    [Parameter(Mandatory = $true)]
    [string]$OutputRoot,

    [switch]$ServiceStopped
)

$ErrorActionPreference = "Stop"

if (-not $ServiceStopped) {
    throw "Pass -ServiceStopped only after all writers have stopped."
}

$database = [IO.Path]::GetFullPath($DatabasePath)
$media = [IO.Path]::GetFullPath($MediaRoot)
$output = [IO.Path]::GetFullPath($OutputRoot)

if (-not (Test-Path -LiteralPath $database -PathType Leaf)) {
    throw "Database path does not exist: $database"
}
if (-not (Test-Path -LiteralPath $media -PathType Container)) {
    throw "Media root does not exist: $media"
}
if (Test-Path -LiteralPath $output) {
    throw "Backup output already exists: $output"
}

New-Item -ItemType Directory -Path $output | Out-Null
Copy-Item -LiteralPath $database -Destination (Join-Path $output "all2api.db")

foreach ($suffix in @("-wal", "-shm")) {
    $sidecar = $database + $suffix
    if (Test-Path -LiteralPath $sidecar -PathType Leaf) {
        Copy-Item -LiteralPath $sidecar -Destination (Join-Path $output ("all2api.db" + $suffix))
    }
}

$mediaDestination = Join-Path $output "media-assets"
New-Item -ItemType Directory -Path $mediaDestination | Out-Null
Get-ChildItem -LiteralPath $media -Force | Copy-Item -Destination $mediaDestination -Recurse -Force

function New-ManifestEntry {
    param(
        [Parameter(Mandatory = $true)]
        [IO.FileInfo]$File,

        [Parameter(Mandatory = $true)]
        [string]$RelativePath
    )

    $hash = Get-FileHash -LiteralPath $File.FullName -Algorithm SHA256
    return [ordered]@{
        path   = $RelativePath
        bytes  = [int64]$File.Length
        sha256 = $hash.Hash.ToLowerInvariant()
    }
}

$databaseEntries = @(
    Get-ChildItem -LiteralPath $output -File |
        Where-Object { $_.Name -like "all2api.db*" } |
        Sort-Object Name |
        ForEach-Object { New-ManifestEntry -File $_ -RelativePath $_.Name }
)
$mediaEntries = @(
    Get-ChildItem -LiteralPath $mediaDestination -Recurse -File |
        Sort-Object FullName |
        ForEach-Object {
            $relative = [IO.Path]::GetRelativePath($output, $_.FullName).Replace("\", "/")
            New-ManifestEntry -File $_ -RelativePath $relative
        }
)

$manifest = [ordered]@{
    format = 1
    created_at = [DateTime]::UtcNow.ToString("o")
    database = $databaseEntries
    media = $mediaEntries
    credential_master_key_external = $true
}
$manifest | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $output "manifest.json") -Encoding ASCII

Write-Output ("Go state backup created: database_files={0}, media_files={1}" -f $databaseEntries.Count, $mediaEntries.Count)
