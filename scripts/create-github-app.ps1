#requires -Version 7
<#
Creates the controller's GitHub App using the browser manifest flow, then installs it on an explicit repository.
The private key goes directly from GitHub to the specified Key Vault; it is never written to disk or logged.
The local infra/github-app.auto.tfvars contains only client and installation IDs. Do not commit that file.
Use -OwnerType Organization to create an organisation-owned app; GitHub.com is the default host.
#>
param(
    [Parameter(Mandatory)][string]$VaultName,
    [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9_-]+$')][string]$Owner,
    [Parameter(Mandatory)][ValidatePattern('^[A-Za-z0-9_.-]+$')][string]$Repo,
    [string]$AppName = 'aca-sandbox-runners-controller',
    [ValidateSet('User', 'Organization')][string]$OwnerType = 'User',
    [ValidatePattern('^[A-Za-z0-9.-]+$')][string]$GitHubHost = 'github.com',
    [ValidateRange(1024, 65535)][int]$Port = 8765
)
$ErrorActionPreference = 'Stop'
$githubBase = "https://$GitHubHost"
$apiBase = if ($GitHubHost -eq 'github.com') { 'https://api.github.com' } else { "$githubBase/api/v3" }
$redirect = "http://localhost:$Port/callback"

# Discover both the vault URI and token audience from the selected Azure cloud before creating the app.
$vaultUri = az keyvault show --name $VaultName --query properties.vaultUri -o tsv --only-show-errors
if ($LASTEXITCODE -ne 0 -or -not $vaultUri) { throw 'Could not resolve the Key Vault URI.' }
$vaultUri = $vaultUri.Trim().TrimEnd('/')
$vaultDns = az cloud show --query endpoints.keyvaultDns -o tsv --only-show-errors
if ($LASTEXITCODE -ne 0 -or -not $vaultDns) { throw 'Could not resolve the Key Vault token audience.' }
$vaultAudience = "https://$($vaultDns.Trim().TrimStart('.').TrimEnd('/'))"

# Repository scale-set registration needs Administration: write (Metadata: read is implicit).
$manifest = @{
    name = $AppName; url = "$githubBase/$Owner/$Repo"; public = $false
    redirect_url = $redirect
    hook_attributes = @{ url = 'https://example.invalid/unused'; active = $false }
    default_permissions = @{ administration = 'write' }
} | ConvertTo-Json -Depth 5 -Compress

$state = [guid]::NewGuid().ToString('N')
$manifestUrl = if ($OwnerType -eq 'Organization') { "$githubBase/organizations/$Owner/settings/apps/new" } else { "$githubBase/settings/apps/new" }
$encodedManifest = [System.Net.WebUtility]::HtmlEncode($manifest)
$page = @"
<!doctype html><body onload="document.forms[0].submit()">
<form method="post" action="$manifestUrl`?state=$state">
<input type="hidden" name="manifest" value="$encodedManifest">
<noscript><button>Create GitHub App</button></noscript></form>Redirecting to GitHub...</body>
"@

function Send([System.Net.HttpListenerContext]$ctx, [string]$html) {
    $bytes = [Text.Encoding]::UTF8.GetBytes($html)
    $ctx.Response.ContentType = 'text/html; charset=utf-8'
    $ctx.Response.OutputStream.Write($bytes, 0, $bytes.Length)
    $ctx.Response.Close()
}

$http = [System.Net.HttpListener]::new()
$http.Prefixes.Add("http://localhost:$Port/")
$http.Start()
$code = $null
try {
    Start-Process "http://localhost:$Port/"
    Write-Host 'Waiting for the GitHub App to be created in your browser (Ctrl+C to abort)...'
    while (-not $code) {
        $ctx = $http.GetContext()
        if ($ctx.Request.Url.AbsolutePath -ne '/callback') { Send $ctx $page; continue }
        if ($ctx.Request.QueryString['state'] -ne $state) { Send $ctx 'state mismatch'; continue }
        $code = $ctx.Request.QueryString['code']
        Send $ctx 'GitHub App created. Return to the terminal.'
    }
} finally { $http.Stop(); $http.Close() }

try {
    $app = Invoke-RestMethod -Method Post -Uri "$apiBase/app-manifests/$code/conversions" -Headers @{ Accept = 'application/vnd.github+json' }
} catch { throw 'GitHub App manifest conversion failed.' }

$token = az account get-access-token --resource $vaultAudience --query accessToken -o tsv --only-show-errors
if ($LASTEXITCODE -ne 0 -or -not $token) { throw 'Could not acquire the Key Vault access token.' }
$kvToken = $token.Trim()
try {
    Invoke-RestMethod -Method Put -Uri "$vaultUri/secrets/github-app-private-key?api-version=7.4" `
        -Headers @{ Authorization = "Bearer $kvToken" } -ContentType 'application/json' -Body (@{ value = $app.pem } | ConvertTo-Json) | Out-Null
} catch { throw 'Could not store the GitHub App private key in Key Vault.' }
finally { $kvToken = $null; $token = $null }
Write-Host "Private key stored in Key Vault $VaultName (secret github-app-private-key)."

Write-Host "Install the app on $Owner/$Repo only (choose 'Only select repositories'):"
$installUrl = "$githubBase/apps/$($app.slug)/installations/new"
Write-Host "  $installUrl"
Start-Process $installUrl

function New-AppJwt([string]$ClientId, [string]$Pem) {
    $b64 = { param($b) [Convert]::ToBase64String($b).TrimEnd('=').Replace('+', '-').Replace('/', '_') }
    $now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
    $head = & $b64 ([Text.Encoding]::UTF8.GetBytes((@{ alg = 'RS256'; typ = 'JWT' } | ConvertTo-Json -Compress)))
    $body = & $b64 ([Text.Encoding]::UTF8.GetBytes((@{ iat = $now - 60; exp = $now + 540; iss = $ClientId } | ConvertTo-Json -Compress)))
    $rsa = [Security.Cryptography.RSA]::Create()
    try {
        $rsa.ImportFromPem($Pem)
        $sig = $rsa.SignData([Text.Encoding]::UTF8.GetBytes("$head.$body"), [Security.Cryptography.HashAlgorithmName]::SHA256, [Security.Cryptography.RSASignaturePadding]::Pkcs1)
        "$head.$body." + (& $b64 $sig)
    } finally { $rsa.Dispose() }
}

$installation = $null
try {
    for ($i = 0; $i -lt 60 -and -not $installation; $i++) {
        try {
            $installation = Invoke-RestMethod -Uri "$apiBase/repos/$Owner/$Repo/installation" `
                -Headers @{ Authorization = "Bearer $(New-AppJwt $app.client_id $app.pem)"; Accept = 'application/vnd.github+json' }
        } catch {
            if ($_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq 404) { Start-Sleep -Seconds 5 }
            else { throw 'Could not query the GitHub App installation.' }
        }
    }
} finally { $app.pem = $null }
if (-not $installation) { throw "App not installed on $Owner/$Repo after 5 minutes; install it, then set github_app_installation_id manually." }

$tfvars = Join-Path $PSScriptRoot '../infra/github-app.auto.tfvars'
@"
github_app_client_id       = "$($app.client_id)"
github_app_installation_id = $($installation.id)
"@ | Set-Content -Path $tfvars -Encoding utf8
Write-Host "Wrote local $tfvars (app '$($app.slug)', installation $($installation.id)). Do not commit it."
Write-Host 'For workflow deployment, set RUNNER_GITHUB_APP_CLIENT_ID and RUNNER_GITHUB_APP_INSTALLATION_ID in the selected GitHub environment, then deploy with deploy_controller=true.'
