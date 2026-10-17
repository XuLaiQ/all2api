[CmdletBinding()]
param(
    [string]$OutputRoot = ""
)

$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($OutputRoot)) {
    $OutputRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-release-" + [guid]::NewGuid().ToString("N"))
}
$output = [IO.Path]::GetFullPath($OutputRoot)
if (Test-Path -LiteralPath $output) {
    throw "Output directory already exists: $output"
}
New-Item -ItemType Directory -Path $output | Out-Null

$oldCgo = $env:CGO_ENABLED
$env:CGO_ENABLED = "0"
$binaries = @(
    @{ Name = "all2api"; Package = "./cmd/all2api" },
    @{ Name = "doubao-browser-worker"; Package = "./cmd/doubao-browser-worker" },
    @{ Name = "media-worker"; Package = "./cmd/media-worker" }
)

try {
    foreach ($item in $binaries) {
        $path = Join-Path $output ($item.Name + ".exe")
        & go build -mod=readonly -trimpath -ldflags="-s -w" -o $path $item.Package
        if ($LASTEXITCODE -ne 0) {
            throw "Go build failed for $($item.Name) with exit code $LASTEXITCODE"
        }
    }

    $moduleRecords = @()
    $goSumPath = Join-Path $workspace "go.sum"
    if (Test-Path -LiteralPath $goSumPath -PathType Leaf) {
        $seenModules = @{}
        foreach ($line in Get-Content -LiteralPath $goSumPath -Encoding UTF8) {
            $parts = ([string]$line).Trim() -split '\s+'
            if ($parts.Count -lt 2 -or $parts[0] -eq "") { continue }
            $modulePath = [string]$parts[0]
            $version = [string]$parts[1]
            if ($version.EndsWith("/go.mod")) {
                $version = $version.Substring(0, $version.Length - 7)
            }
            $key = $modulePath + "@" + $version
            if (-not $seenModules.ContainsKey($key)) {
                $seenModules[$key] = $true
                $moduleRecords += [ordered]@{ path = $modulePath; version = $version; source = "go.sum" }
            }
        }
    }
    $binaryRecords = @(
        Get-ChildItem -LiteralPath $output -Filter "*.exe" -File | Sort-Object Name | ForEach-Object {
            $hash = Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256
            [ordered]@{ name = $_.Name; bytes = [int64]$_.Length; sha256 = $hash.Hash.ToLowerInvariant() }
        }
    )
    $manifest = [ordered]@{
        format = "all2api-go-release-v1"
        generated_at = [DateTime]::UtcNow.ToString("o")
        go_version = (& go version).Trim()
        cgo_enabled = "0"
        source_root = "repository"
        binaries = $binaryRecords
        modules = $moduleRecords
    }
    $manifest | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $output "go-sbom.json") -Encoding UTF8
    Write-Output ("Go release artifacts and SBOM generated: {0}" -f $output)
} finally {
    if ($null -eq $oldCgo) {
        Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue
    } else {
        $env:CGO_ENABLED = $oldCgo
    }
}
