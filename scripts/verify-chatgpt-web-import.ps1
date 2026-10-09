[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$AccountFile,
    [string]$DatabasePath = ".\services\api\data\all2api.db",
    [string]$ChatGPTProxy = ""
)

$ErrorActionPreference = "Stop"
$workspace = Split-Path -Parent $PSScriptRoot
$sourceAccountPath = [IO.Path]::GetFullPath($AccountFile)
$sourceDatabasePath = [IO.Path]::GetFullPath((Join-Path $workspace $DatabasePath))
if (-not (Test-Path -LiteralPath $sourceAccountPath -PathType Leaf)) { throw "account JSON does not exist" }
if (-not (Test-Path -LiteralPath $sourceDatabasePath -PathType Leaf)) { throw "source SQLite database does not exist" }

$tempRoot = Join-Path ([IO.Path]::GetTempPath()) ("all2api-web-import-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempRoot | Out-Null
Copy-Item -LiteralPath $sourceDatabasePath -Destination (Join-Path $tempRoot "all2api.db")
$binary = Join-Path $tempRoot "all2api.exe"
$process = $null
$oldEnvironment = @{}
$evidence = [ordered]@{
    format = "all2api-chatgpt-web-import-v1"
    source_account_count = 0
    source_web_entries = 0
    import_status = 0
    import_error_code = ""
    provision_schema_status = 0
    imported_added = 0
    imported_skipped = 0
    key_status = 0
    key_error_code = ""
    refresh_status = 0
    refresh_error_code = ""
    models_status = 0
    model_count = 0
    selected_model = ""
    chat_status = 0
    chat_text_length = 0
    chat_error_code = ""
    proxy_configured = -not [string]::IsNullOrWhiteSpace($ChatGPTProxy)
    database = "disposable SQLite copy"
    secrets_logged = $false
}

function Send-HTTP {
    param(
        [string]$Method,
        [string]$Url,
        [string]$Authorization = "",
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
        if ($Json) { $request.Content = [System.Net.Http.StringContent]::new([string]$Json, [Text.Encoding]::UTF8, "application/json") }
        $response = $client.SendAsync($request).GetAwaiter().GetResult()
        $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        return [pscustomobject]@{ Status = [int]$response.StatusCode; Body = $text }
    } catch {
        return [pscustomobject]@{ Status = 0; Body = "" }
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

try {
    & go build -trimpath -o $binary (Join-Path $workspace "cmd\all2api")
    if ($LASTEXITCODE -ne 0) { throw "temporary Go gateway build failed" }

    $port = Get-Random -Minimum 19020 -Maximum 19120
    $base = "http://127.0.0.1:$port"
    $adminToken = "wbt_web_import_management_token_20261009_long"
    $environment = @{
        A2A_ENV = "development"
        A2A_HOST = "127.0.0.1"
        A2A_PORT = [string]$port
        A2A_DB_PATH = Join-Path $tempRoot "all2api.db"
        A2A_STATE_PATH = Join-Path $tempRoot "state.json"
        A2A_SESSION_SECRET = "web-import-session-secret-20261009"
        A2A_CREDENTIAL_MASTER_KEY = ""
        A2A_ADMIN_USERNAME = "admin"
        A2A_ADMIN_PASSWORD = "web-import-admin-password-20261009"
        A2A_ADMIN_TOKEN = $adminToken
        A2A_CHATGPT_WEB_BASE = "https://chatgpt.com"
        A2A_CHATGPT_PROXY = $ChatGPTProxy
    }
    foreach ($name in $environment.Keys) {
        $oldEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        [Environment]::SetEnvironmentVariable($name, [string]$environment[$name], "Process")
    }
    $stdout = Join-Path $tempRoot "stdout.log"
    $stderr = Join-Path $tempRoot "stderr.log"
    $process = Start-Process -FilePath $binary -WorkingDirectory $workspace -RedirectStandardOutput $stdout -RedirectStandardError $stderr -WindowStyle Hidden -PassThru
    foreach ($name in $oldEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], "Process") }

    $health = $null
    for ($attempt = 0; $attempt -lt 100; $attempt++) {
        if ($process.HasExited) { throw "temporary Go gateway exited before health" }
        $health = Send-HTTP -Method "GET" -Url "$base/admin/api/healthz?detail=true"
        if ($health.Status -eq 200) { break }
        Start-Sleep -Milliseconds 200
    }
    if ($null -eq $health -or $health.Status -ne 200) { throw "temporary Go gateway health timeout" }

    $schemaResponse = Send-HTTP -Method "GET" -Url "$base/admin/api/channels/chatgpt/provision-schema" -Authorization ("Bearer " + $adminToken)
    $evidence.provision_schema_status = $schemaResponse.Status

    $source = Get-Content -LiteralPath $sourceAccountPath -Raw -Encoding UTF8 | ConvertFrom-Json
    $sourceAccounts = @($source.accounts)
    $evidence.source_account_count = $sourceAccounts.Count
    $entries = @()
    $sourceIndex = 0
    foreach ($account in $sourceAccounts) {
        $sourceIndex++
        $credentials = $account.credentials
        $entry = [ordered]@{}
        foreach ($key in @("access_token", "refresh_token", "id_token", "client_id", "chatgpt_account_id", "chatgpt_user_id", "email", "organization_id", "plan_type", "expires_at")) {
            $property = $credentials.PSObject.Properties[$key]
            if ($null -ne $property -and $null -ne $property.Value) { $entry[$key] = $property.Value }
        }
        if (-not $entry.Contains("access_token")) { continue }
        $entry["auth_mode"] = "web"
        if ($entry.Contains("chatgpt_account_id")) { $entry["account_id"] = [string]$entry["chatgpt_account_id"] }
        $entry["name"] = "sub2api-web-import-$sourceIndex"
        $entries += [pscustomobject]$entry
    }
    $evidence.source_web_entries = $entries.Count
    if ($entries.Count -eq 0) { throw "source JSON contained no Web access token" }

    $importBody = [ordered]@{
        flow = "token-import"
        payload = [ordered]@{ accounts = @($entries) }
        idempotency_key = "sub2api-web-import-20261009-203027"
    } | ConvertTo-Json -Depth 12 -Compress
    $importResponse = Send-HTTP -Method "POST" -Url "$base/admin/api/channels/chatgpt/accounts/provision/import" -Authorization ("Bearer " + $adminToken) -Json $importBody
    $evidence.import_status = $importResponse.Status
    if ($importResponse.Status -lt 200 -or $importResponse.Status -ge 300) {
        $evidence.import_error_code = Error-Code $importResponse.Body
        $evidence | ConvertTo-Json -Compress
        return
    }
    try {
        $importPayload = $importResponse.Body | ConvertFrom-Json
        if ($importPayload.data) {
            $evidence.imported_added = [int]$importPayload.data.added
            $evidence.imported_skipped = [int]$importPayload.data.skipped
        }
    } catch {
    }

    $accountCredential = @($source.accounts)[0].credentials
    $chatGPTAccountID = [string]$accountCredential.chatgpt_account_id
    if (-not [string]::IsNullOrWhiteSpace($chatGPTAccountID)) {
        $refreshURL = "$base/admin/api/accounts/chatgpt:$([uri]::EscapeDataString($chatGPTAccountID))/refresh"
        $refreshResponse = Send-HTTP -Method "POST" -Url $refreshURL -Authorization ("Bearer " + $adminToken)
        $evidence.refresh_status = $refreshResponse.Status
        if ($refreshResponse.Status -lt 200 -or $refreshResponse.Status -ge 300) { $evidence.refresh_error_code = Error-Code $refreshResponse.Body }
    }

    $keyBody = (@{ name = "web-import-test-20261009"; channels = @("chatgpt"); models = @("chatgpt/*"); limit_rpm = 60 } | ConvertTo-Json -Compress)
    $keyResponse = Send-HTTP -Method "POST" -Url "$base/admin/api/keys" -Authorization ("Bearer " + $adminToken) -Json $keyBody
    $evidence.key_status = $keyResponse.Status
    if ($keyResponse.Status -lt 200 -or $keyResponse.Status -ge 300) {
        $evidence.key_error_code = Error-Code $keyResponse.Body
        $evidence | ConvertTo-Json -Compress
        return
    }
    $keyPayload = $keyResponse.Body | ConvertFrom-Json
    $gatewayKey = [string]$keyPayload.key
    if ([string]::IsNullOrWhiteSpace($gatewayKey)) { throw "temporary gateway key creation failed" }

    $modelsResponse = Send-HTTP -Method "GET" -Url "$base/v1/models" -Authorization ("Bearer " + $gatewayKey)
    $evidence.models_status = $modelsResponse.Status
    $modelPayload = $modelsResponse.Body | ConvertFrom-Json
    $modelIDs = @($modelPayload.data | ForEach-Object { [string]$_.id })
    $evidence.model_count = $modelIDs.Count
    if ($modelIDs.Count -gt 0) {
        $evidence.selected_model = $modelIDs[0]
        $chatBody = (@{ model = $modelIDs[0]; messages = @(@{ role = "user"; content = "Reply with OK." }); stream = $false; max_tokens = 16 } | ConvertTo-Json -Depth 8 -Compress)
        $chatResponse = Send-HTTP -Method "POST" -Url "$base/v1/chat/completions" -Authorization ("Bearer " + $gatewayKey) -Json $chatBody
        $evidence.chat_status = $chatResponse.Status
        try {
            $chatPayload = $chatResponse.Body | ConvertFrom-Json
            if ($chatPayload.error) {
                $evidence.chat_error_code = [string]$chatPayload.error.code
            } else {
                $evidence.chat_text_length = ([string]$chatPayload.choices[0].message.content).Length
            }
        } catch {
            $evidence.chat_error_code = "invalid_response"
        }
    } else {
        $evidence.chat_error_code = "no_models"
    }
    $evidence | ConvertTo-Json -Compress
} finally {
    if ($null -ne $process) {
        if (-not $process.HasExited) { $process.Kill() }
        $process.WaitForExit()
        $process.Dispose()
    }
    foreach ($name in $oldEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $oldEnvironment[$name], "Process") }
    if (Test-Path -LiteralPath $tempRoot) { Remove-Item -LiteralPath $tempRoot -Recurse -Force }
}
