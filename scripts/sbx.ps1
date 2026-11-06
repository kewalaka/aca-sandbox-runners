#requires -Version 5.1
# ACA Sandbox data-plane helpers (REST) for disk-image registration. Auth: Entra token for https://dynamicsessions.io
Set-StrictMode -Version Latest

$script:SbxCtx = @{ ApiVersion = '2026-02-01-preview'; Token = $null; TokenExpiry = [datetime]::MinValue }

function Initialize-Sbx {
    param(
        [Parameter(Mandatory)][string]$Endpoint,
        [Parameter(Mandatory)][string]$SubscriptionId,
        [Parameter(Mandatory)][string]$ResourceGroup,
        [Parameter(Mandatory)][string]$Group
    )
    $uri = [uri]$Endpoint
    if (-not $uri.IsAbsoluteUri -or $uri.Scheme -ne 'https' -or $uri.UserInfo -or $uri.Query -or $uri.Fragment) { throw 'Endpoint must be an absolute HTTPS URL without credentials, query or fragment.' }
    $script:SbxCtx.Token = $null
    $script:SbxCtx.TokenExpiry = [datetime]::MinValue
    $script:SbxCtx.SubscriptionId = $SubscriptionId
    $script:SbxCtx.Endpoint = $Endpoint.TrimEnd('/')
    $script:SbxCtx.Base = "/subscriptions/$SubscriptionId/resourceGroups/$ResourceGroup/sandboxGroups/$Group"
}

function Get-SbxToken {
    # Cache the token for ~45 minutes; az returns a fresh one when needed.
    if (-not $script:SbxCtx.Token -or ([datetime]::UtcNow -gt $script:SbxCtx.TokenExpiry)) {
        $t = az account get-access-token --subscription $script:SbxCtx.SubscriptionId --resource https://dynamicsessions.io --query accessToken -o tsv --only-show-errors
        if ($LASTEXITCODE -ne 0 -or -not $t) { throw 'az account get-access-token failed' }
        $script:SbxCtx.Token = $t.Trim()
        $script:SbxCtx.TokenExpiry = [datetime]::UtcNow.AddMinutes(45)
    }
    $script:SbxCtx.Token
}

function Invoke-Sbx {
    param(
        [Parameter(Mandatory)][string]$Method,
        [Parameter(Mandatory)][string]$Path,
        $Body,
        [int]$TimeoutSec = 600,
        [switch]$AllowNotFound
    )
    $uri = "$($script:SbxCtx.Endpoint)$($script:SbxCtx.Base)$Path" + $(if ($Path.Contains('?')) { '&' } else { '?' }) + "api-version=$($script:SbxCtx.ApiVersion)"
    $req = @{
        Method = $Method; Uri = $uri; TimeoutSec = $TimeoutSec
        Headers = @{ Authorization = "Bearer $(Get-SbxToken)" }
    }
    if ($null -ne $Body) { $req.Body = ($Body | ConvertTo-Json -Depth 10 -Compress); $req.ContentType = 'application/json' }
    try { Invoke-RestMethod @req }
    catch {
        $code = $_.Exception.Response.StatusCode.value__
        if ($AllowNotFound -and $code -eq 404) { return $null }
        $detail = $_.ErrorDetails.Message
        throw "Sandbox API $Method $Path failed ($code): $detail"
    }
}

function New-SbxDiskImage {
    param([Parameter(Mandatory)][string]$BaseImage, [string]$Name, [string]$Username, [string]$Token, [string[]]$Entrypoint, [string[]]$Cmd)
    $body = @{ image = @{ base = $BaseImage } }
    if ($Entrypoint) { $body.image.entrypoint = $Entrypoint }
    if ($Cmd) { $body.image.cmd = $Cmd }
    if ($Name) { $body.labels = @{ name = $Name } }
    if ($Username) { $body.registryCredentials = @{ username = $Username; token = $Token } }
    Invoke-Sbx PUT '/diskimages' $body
}

function Wait-SbxDiskImage {
    param([Parameter(Mandatory)][string]$Id, [int]$TimeoutSec = 900)
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($true) {
        $i = Invoke-Sbx GET "/diskimages/$Id"
        $s = $i.status.state
        if ($s -in 'Ready', 'Succeeded') { return $i }
        if ($s -eq 'Failed') { throw "Disk image $Id failed: $($i.status.message)" }
        if ($sw.Elapsed.TotalSeconds -gt $TimeoutSec) { throw "Disk image $Id not ready after ${TimeoutSec}s (state=$s)" }
        Start-Sleep -Seconds 3
    }
}
