# Build the production Dockerfile and verify the image boundary: the three Go
# binaries are present and no Python runtime is shipped.
# Exit codes: 0 verified, 1 a check failed, 2 Docker unavailable (unverified).
$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$tag = "all2api-image-boundary:verify"

# Windows PowerShell 5.1 turns native stderr into a terminating error under
# ErrorActionPreference=Stop, so probe the daemon with errors tolerated.
$dockerAvailable = $false
try {
    $ErrorActionPreference = "Continue"
    $null = docker info --format "{{.ServerVersion}}" 2>&1
    $dockerAvailable = $LASTEXITCODE -eq 0
} catch {
    $dockerAvailable = $false
} finally {
    $ErrorActionPreference = "Stop"
}
if (-not $dockerAvailable) {
    Write-Output "UNVERIFIED: Docker daemon is unavailable"
    exit 2
}

Push-Location $workspace
try {
    docker build -t $tag -f Dockerfile .
    if ($LASTEXITCODE -ne 0) {
        Write-Output "FAIL: docker build failed"
        exit 1
    }
    $check = "test -x /usr/local/bin/all2api && test -x /usr/local/bin/doubao-browser-worker && test -x /usr/local/bin/media-worker && ! command -v python3 && ! command -v python"
    docker run --rm --entrypoint /bin/sh $tag -c $check
    if ($LASTEXITCODE -ne 0) {
        Write-Output "FAIL: image is missing a Go binary or ships a Python runtime"
        exit 1
    }
    Write-Output "PASS: image contains only the Go runtime boundary"
    exit 0
}
finally {
    docker rmi $tag 2>$null | Out-Null
    Pop-Location
}
