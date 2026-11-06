#requires -Version 5.1
# Registers an OCI image as a sandbox-group disk image and waits until it is Ready.
# ACR images: the caller's Entra identity (needs AcrPull/AcrPush) mints a short-lived ACR refresh token that is
# handed to the sandbox data plane. The disk-image API's managed-identity pull (managedIdentityClientId) returned
# 401 RegistryAuthFailed in the current preview even with AcrPull, so no identity or admin user is stored.
# Supply Endpoint, SubscriptionId, ResourceGroup and Group for the target deployment; Name must start with runner-.
# Use RegistryName for ACR endpoints that are not recognisable from the image hostname.
# Other private registries: -RegistryUsername/-RegistryToken (token also from $env:REGISTRY_TOKEN).
param(
    [Parameter(Mandatory)][string]$Image,
    [Parameter(Mandatory)][ValidatePattern('^runner-.+')][string]$Name,
    [Parameter(Mandatory)][string]$Endpoint,
    [Parameter(Mandatory)][string]$SubscriptionId,
    [Parameter(Mandatory)][string]$ResourceGroup,
    [Parameter(Mandatory)][string]$Group,
    [string]$RegistryName,
    [string]$RegistryUsername,
    [string]$RegistryToken = $env:REGISTRY_TOKEN
)
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/sbx.ps1"
Initialize-Sbx -Endpoint $Endpoint -SubscriptionId $SubscriptionId -ResourceGroup $ResourceGroup -Group $Group
if (-not $RegistryUsername -and ($RegistryName -or $Image -match '^([^/]+)\.azurecr\.(io|us|cn)/')) {
    if (-not $RegistryName) { $RegistryName = $Matches[1] }
    $RegistryUsername = '00000000-0000-0000-0000-000000000000'
    $token = az acr login -n $RegistryName --subscription $SubscriptionId --expose-token --query accessToken -o tsv --only-show-errors
    if ($LASTEXITCODE -ne 0 -or -not $token) { throw "Could not mint an ACR token for $RegistryName" }
    $RegistryToken = $token.Trim()
}
if ([bool]$RegistryUsername -ne [bool]$RegistryToken) { throw 'RegistryUsername and RegistryToken must be supplied together.' }
$sw = [Diagnostics.Stopwatch]::StartNew()
$img = New-SbxDiskImage -BaseImage $Image -Name $Name -Username $RegistryUsername -Token $RegistryToken
$img = Wait-SbxDiskImage $img.id
Write-Host "disk image $($img.id) ready in $([math]::Round($sw.Elapsed.TotalSeconds,1))s ($($img.sizeInMB) MB)"
$img.id
