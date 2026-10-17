$ErrorActionPreference = "Stop"

$workspace = Split-Path -Parent $PSScriptRoot
$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-clean-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempRoot | Out-Null

$env:GOPROXY = if ($env:GOPROXY) { $env:GOPROXY } else { "https://proxy.golang.org,direct" }
$env:GOSUMDB = if ($env:GOSUMDB) { $env:GOSUMDB } else { "sum.golang.org" }
$binary = Join-Path $tempRoot "all2api.exe"
go build -mod=readonly -trimpath -o $binary (Join-Path $workspace "cmd/all2api")

$forbidden = @("F:\token-p", "wb2api", "doubao2api", "chatgpt2api", ":7863", ":7864", ":9090")
$runtimeFiles = @(
    (Join-Path $workspace "cmd"),
    (Join-Path $workspace "internal"),
    (Join-Path $workspace "Dockerfile.golang"),
    (Join-Path $workspace "docker-compose.go.yml")
)
foreach ($path in $runtimeFiles) {
    $files = if (Test-Path -LiteralPath $path -PathType Container) { Get-ChildItem -LiteralPath $path -Recurse -File } else { Get-Item -LiteralPath $path }
    foreach ($file in $files) {
        $text = Get-Content -LiteralPath $file.FullName -Raw -ErrorAction Stop
        foreach ($token in $forbidden) {
            if ($text.IndexOf($token, [StringComparison]::OrdinalIgnoreCase) -ge 0) {
                throw "Go runtime boundary contains forbidden token '$token': $($file.FullName)"
            }
        }
    }
}

$env:A2A_HOST = "127.0.0.1"
$env:A2A_PORT = "18991"
$env:A2A_DB_PATH = Join-Path $tempRoot "all2api.db"
$env:A2A_STATE_PATH = Join-Path $tempRoot "state.json"
$env:A2A_SESSION_SECRET = "0123456789abcdef0123456789abcdef"
$env:A2A_ADMIN_PASSWORD = "correct horse battery staple"
$env:A2A_ADMIN_USERNAME = "admin"
$stdout = Join-Path $tempRoot "stdout.log"
$stderr = Join-Path $tempRoot "stderr.log"
$processInfo = [System.Diagnostics.ProcessStartInfo]::new()
$processInfo.FileName = $binary
$processInfo.WorkingDirectory = $workspace
$processInfo.UseShellExecute = $false
$processInfo.RedirectStandardOutput = $true
$processInfo.RedirectStandardError = $true
$process = [System.Diagnostics.Process]::new()
$process.StartInfo = $processInfo
$null = $process.Start()
$stdoutTask = $process.StandardOutput.ReadToEndAsync()
$stderrTask = $process.StandardError.ReadToEndAsync()
try {
    $response = $null
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($process.HasExited) { throw "Go process exited with code $($process.ExitCode)" }
        try {
            $response = & curl.exe --noproxy "*" -sS --max-time 1 "http://127.0.0.1:18991/admin/api/healthz?detail=true" 2>$null
            if ($LASTEXITCODE -ne 0) { $response = $null; throw "health probe failed" }
            break
        } catch {
            Start-Sleep -Milliseconds 250
        }
    }
    if ($null -eq $response) { throw "Go healthz did not return HTTP 200: curl probe timed out" }
    $body = ([string]$response) | ConvertFrom-Json
    if ($body.status -ne "ok" -or $body.database -ne "ok" -or $body.checks.storage.schema_version -ne 11) { throw "Go healthz returned an invalid storage result: $response" }
    Write-Output "Go clean build/run verification passed."
} finally {
    if (-not $process.HasExited) { $process.Kill() }
    $process.WaitForExit()
    $stdoutTask.GetAwaiter().GetResult() | Set-Content -LiteralPath $stdout -Encoding UTF8
    $stderrTask.GetAwaiter().GetResult() | Set-Content -LiteralPath $stderr -Encoding UTF8
}
