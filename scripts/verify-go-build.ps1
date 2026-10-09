$ErrorActionPreference = "Stop"

$env:GOPROXY = if ($env:GOPROXY) { $env:GOPROXY } else { "https://proxy.golang.org,direct" }
$env:GOSUMDB = if ($env:GOSUMDB) { $env:GOSUMDB } else { "sum.golang.org" }

go test -mod=readonly ./...
go vet -mod=readonly ./...
$goBuildRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-build-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $goBuildRoot | Out-Null
go build -mod=readonly -o (Join-Path $goBuildRoot "all2api.exe") ./cmd/all2api
go build -mod=readonly -o (Join-Path $goBuildRoot "doubao-browser-worker.exe") ./cmd/doubao-browser-worker
go build -mod=readonly -o (Join-Path $goBuildRoot "media-worker.exe") ./cmd/media-worker

Write-Output "Go build and test verification passed."
