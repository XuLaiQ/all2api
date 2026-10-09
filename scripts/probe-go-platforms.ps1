[CmdletBinding()]
param(
    [string]$DatabasePath = ".\services\api\data\all2api.db",
    [string]$EvidencePath = "",
    [string]$ChatGPTProxy = ""
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Net.Http
$workspace = Split-Path -Parent $PSScriptRoot
$sourceDatabase = [IO.Path]::GetFullPath((Join-Path $workspace $DatabasePath))
if (-not (Test-Path -LiteralPath $sourceDatabase -PathType Leaf)) {
    throw "Probe database does not exist: $sourceDatabase"
}

$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-go-real-probe-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempRoot | Out-Null
Copy-Item -LiteralPath $sourceDatabase -Destination (Join-Path $tempRoot "all2api.db")
$binary = Join-Path $tempRoot "all2api.exe"
$port = Get-Random -Minimum 18996 -Maximum 19900
$base = "http://127.0.0.1:$port"
$adminToken = "wbt_real_probe_management_token_20261009_long"
$oldEnvironment = @{}
$evidence = [ordered]@{
    format = "all2api-go-platform-probe-v1"
    checked_at = [DateTime]::UtcNow.ToString("o")
    database = "disposable SQLite copy"
    secrets_logged = $false
    chatgpt_proxy_configured = -not [string]::IsNullOrWhiteSpace($ChatGPTProxy)
    health = [ordered]@{ status = 0; schema_version = 0 }
    channels = @()
}
$environment = @{
    A2A_ENV = "development"
    A2A_HOST = "127.0.0.1"
    A2A_PORT = [string]$port
    A2A_DB_PATH = Join-Path $tempRoot "all2api.db"
    A2A_STATE_PATH = Join-Path $tempRoot "state.json"
    A2A_SESSION_SECRET = "real-probe-session-secret-0123456789012345"
    A2A_CREDENTIAL_MASTER_KEY = ""
    A2A_ADMIN_USERNAME = "admin"
    A2A_ADMIN_PASSWORD = "real-probe-admin-password-20261009"
    A2A_ADMIN_TOKEN = $adminToken
    A2A_WB_PLATFORM_BASE = "https://copilot.tencent.com"
    A2A_DOUBAO_PLATFORM_BASE = "https://www.doubao.com"
    A2A_CHATGPT_WEB_BASE = "https://chatgpt.com"
    A2A_CHATGPT_PROXY = $ChatGPTProxy
}
$process = $null

function Send-HTTP {
    param(
        [string]$Method,
        [string]$Url,
        [string]$Authorization = "",
        [string]$Origin = "",
        [string]$Json = ""
    )
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.UseProxy = $false
    $client = [System.Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(120)
    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::new($Method), $Url)
    $response = $null
    try {
        if ($Authorization) { $request.Headers.TryAddWithoutValidation("Authorization", $Authorization) | Out-Null }
        if ($Origin) { $request.Headers.TryAddWithoutValidation("Origin", $Origin) | Out-Null }
        if ($Json) {
            $request.Content = [System.Net.Http.StringContent]::new([string]$Json, [Text.Encoding]::UTF8, "application/json")
        }
        $response = $client.SendAsync($request).GetAwaiter().GetResult()
        $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        return [pscustomobject]@{ Status = [int]$response.StatusCode; Body = $text }
    } catch {
        return [pscustomobject]@{ Status = 0; Body = $_.Exception.Message }
    } finally {
        if ($null -ne $response) { $response.Dispose() }
        $request.Dispose()
        $client.Dispose()
        $handler.Dispose()
    }
}

function Error-Code {
    param([string]$Body)
    try {
        $value = $Body | ConvertFrom-Json
        if ($value.error -and $value.error.code) { return [string]$value.error.code }
        if ($value.code) { return [string]$value.code }
    } catch {
    }
    return "unknown"
}

function Error-Message {
    param([string]$Body)
    try {
        $value = $Body | ConvertFrom-Json
        if ($value.error -and $value.error.message) { return [string]$value.error.message }
        if ($value.message) { return [string]$value.message }
    } catch {
    }
    return "unknown"
}

function New-Key {
    param([string]$Name, [string]$Channel)
    $payload = (@{ name = $Name; channels = @($Channel); models = @("$Channel/*"); limit_rpm = 60 } | ConvertTo-Json -Compress)
    $response = Send-HTTP -Method "POST" -Url "$base/admin/api/keys" -Authorization "Bearer $adminToken" -Origin $base -Json $payload
    if ($response.Status -ne 201) { throw "key creation failed channel=$Channel status=$($response.Status) code=$(Error-Code $response.Body) message=$(Error-Message $response.Body)" }
    $value = $response.Body | ConvertFrom-Json
    return [string]$value.key
}

function Probe-Models {
    param([string]$Channel, [string]$Key)
    $response = Send-HTTP -Method "GET" -Url "$base/v1/models" -Authorization "Bearer $Key"
    if ($response.Status -ne 200) {
        return [pscustomobject]@{ Status = $response.Status; IDs = @(); ErrorCode = (Error-Code $response.Body) }
    }
    $value = $response.Body | ConvertFrom-Json
    $ids = @($value.data | ForEach-Object { [string]$_.id })
    return [pscustomobject]@{ Status = 200; IDs = $ids; ErrorCode = "" }
}

function Probe-Chat {
    param([string]$Channel, [string]$Key, [string]$Model)
    if ([string]::IsNullOrWhiteSpace($Model)) {
        return [pscustomobject]@{ Status = 0; TextLength = 0; ErrorCode = "no_models" }
    }
    $payload = (@{ model = $Model; messages = @(@{ role = "user"; content = "Reply with OK." }); stream = $false; max_tokens = 16 } | ConvertTo-Json -Depth 8 -Compress)
    $response = Send-HTTP -Method "POST" -Url "$base/v1/chat/completions" -Authorization "Bearer $Key" -Json $payload
    if ($response.Status -lt 200 -or $response.Status -ge 300) {
        return [pscustomobject]@{ Status = $response.Status; TextLength = 0; ErrorCode = (Error-Code $response.Body) }
    }
    try {
        $value = $response.Body | ConvertFrom-Json
        $text = [string]$value.choices[0].message.content
        return [pscustomobject]@{ Status = $response.Status; TextLength = $text.Length; ErrorCode = "" }
    } catch {
        return [pscustomobject]@{ Status = $response.Status; TextLength = 0; ErrorCode = "invalid_response" }
    }
}

function New-ChannelEvidence {
    param(
        [string]$Channel,
        [object]$Models,
        [object]$Chat
    )
    $success = $Models.Status -eq 200 -and $Chat.Status -ge 200 -and $Chat.Status -lt 300 -and $Chat.TextLength -gt 0
    $entry = [ordered]@{
        channel = $Channel
        models_status = [int]$Models.Status
        model_count = @($Models.IDs).Count
        chat_status = [int]$Chat.Status
        chat_text_length = [int]$Chat.TextLength
        status = if ($success) { "ok" } else { "failed" }
    }
    if (-not $success) {
        $entry.error_code = if ($Models.Status -ne 200 -and $Models.ErrorCode) { $Models.ErrorCode } elseif ($Chat.ErrorCode) { $Chat.ErrorCode } elseif ($Models.ErrorCode) { $Models.ErrorCode } else { "unknown" }
    }
    return $entry
}

try {
    foreach ($name in $environment.Keys) {
        $oldEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        [Environment]::SetEnvironmentVariable($name, [string]$environment[$name], "Process")
    }
    & go build -mod=readonly -trimpath -o $binary (Join-Path $workspace "cmd\all2api")
    if ($LASTEXITCODE -ne 0) { throw "Go probe build failed" }
    $stdout = Join-Path $tempRoot "stdout.log"
    $stderr = Join-Path $tempRoot "stderr.log"
    $process = Start-Process -FilePath $binary -WorkingDirectory $workspace -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
    $health = $null
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if ($process.HasExited) { throw "Go probe process exited with code $($process.ExitCode)" }
        $healthResponse = Send-HTTP -Method "GET" -Url "$base/admin/api/healthz?detail=true"
        if ($healthResponse.Status -eq 200) { $health = $healthResponse.Body | ConvertFrom-Json; break }
        Start-Sleep -Milliseconds 250
    }
    if ($null -eq $health) { throw "Go probe health timed out" }
    $evidence.health = [ordered]@{ status = 200; schema_version = [int]$health.checks.storage.schema_version }
    Write-Output ("health status=200 schema={0}" -f $evidence.health.schema_version)

    $wbKey = New-Key -Name "real-probe-wb-20261009" -Channel "wb"
    $doubaoKey = New-Key -Name "real-probe-doubao-20261009" -Channel "doubao"
    $chatgptKey = New-Key -Name "real-probe-chatgpt-20261009" -Channel "chatgpt"
    $wbModels = Probe-Models -Channel "wb" -Key $wbKey
    $doubaoModels = Probe-Models -Channel "doubao" -Key $doubaoKey
    $chatgptModels = Probe-Models -Channel "chatgpt" -Key $chatgptKey
    $wbChat = Probe-Chat -Channel "wb" -Key $wbKey -Model (@($wbModels.IDs) | Select-Object -First 1)
    $doubaoChat = Probe-Chat -Channel "doubao" -Key $doubaoKey -Model (@($doubaoModels.IDs) | Select-Object -First 1)
    $chatgptChat = Probe-Chat -Channel "chatgpt" -Key $chatgptKey -Model (@($chatgptModels.IDs) | Select-Object -First 1)
    $evidence.channels = @(
        (New-ChannelEvidence -Channel "wb" -Models $wbModels -Chat $wbChat),
        (New-ChannelEvidence -Channel "doubao" -Models $doubaoModels -Chat $doubaoChat),
        (New-ChannelEvidence -Channel "chatgpt" -Models $chatgptModels -Chat $chatgptChat)
    )
    foreach ($entry in @($evidence.channels)) {
        Write-Output ("channel={0} models={1} chat={2} status={3}" -f $entry.channel, $entry.models_status, $entry.chat_status, $entry.status)
    }
} catch {
    $message = $_.Exception.Message
    if ($message.Length -gt 200) { $message = $message.Substring(0, 200) }
    $evidence.error = $message.Replace([Environment]::NewLine, " ")
    throw
} finally {
    if ($null -ne $process) {
        if (-not $process.HasExited) { $process.Kill() }
        $process.WaitForExit()
        $process.Dispose()
    }
    foreach ($name in $oldEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], "Process") }
    if (-not [string]::IsNullOrWhiteSpace($EvidencePath)) {
        $destination = [IO.Path]::GetFullPath($EvidencePath)
        New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
        $evidence | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $destination -Encoding UTF8
    }
    if (Test-Path -LiteralPath $tempRoot) { Remove-Item -LiteralPath $tempRoot -Recurse -Force }
}

if (@($evidence.channels | Where-Object { $_.status -ne "ok" }).Count -gt 0) {
    exit 2
}
