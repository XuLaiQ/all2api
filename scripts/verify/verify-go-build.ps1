$ErrorActionPreference = "Stop"

$env:GOPROXY = if ($env:GOPROXY) { $env:GOPROXY } else { "https://proxy.golang.org,direct" }
$env:GOSUMDB = if ($env:GOSUMDB) { $env:GOSUMDB } else { "sum.golang.org" }

# Run from the repository root so the caller's working directory does not matter.
$workspace = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Push-Location $workspace
try {
    go test -mod=readonly ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
    go vet -mod=readonly ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
    $goBuildRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-build-" + [guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Path $goBuildRoot | Out-Null
    try {
        foreach ($name in "all2api", "doubao-browser-worker", "media-worker") {
            go build -mod=readonly -o (Join-Path $goBuildRoot "$name.exe") "./cmd/$name"
            if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/$name failed with exit code $LASTEXITCODE" }
        }
    }
    finally {
        Remove-Item -LiteralPath $goBuildRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}
finally {
    Pop-Location
}

Write-Output "Go build and test verification passed."
