<#
.SYNOPSIS
  Builds a release: zips for x64 and ARM64 plus SHA256SUMS.txt in .\release,
  named the way the README's RMM install script expects.
.EXAMPLE
  .\release.ps1 -Version 0.8.0
  # release\OneDriveHelper-v0.8.0-x64.zip
  # release\OneDriveHelper-v0.8.0-arm64.zip
  # release\SHA256SUMS.txt
#>
param(
    [Parameter(Mandatory = $true)][string]$Version
)
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "Version must look like 1.2.3 (got '$Version')" }
$v = "v$Version"

# Vet, test and build both architectures into dist\amd64 and dist\arm64.
& .\build.ps1 -Version $Version
if ($LASTEXITCODE) { exit $LASTEXITCODE }

# Code signing would go here: sign every dist\<arch>\*.exe (signtool sign /fd sha256 /tr <timestamp-url> ...)
# before they are zipped, so the zips and checksums cover the signed files.

$out = Join-Path $PSScriptRoot "release"
New-Item -ItemType Directory -Force $out | Out-Null
$programs = "OneDriveHelper.exe", "OneDriveHelperAgent.exe", "OneDriveHelperConfig.exe"
$zips = @()
foreach ($a in @(@{ Dir = "amd64"; Name = "x64" }, @{ Dir = "arm64"; Name = "arm64" })) {
    $stage = Join-Path $out "stage-$($a.Name)"
    Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory $stage | Out-Null
    foreach ($p in $programs) { Copy-Item (Join-Path "dist\$($a.Dir)" $p) $stage }
    Copy-Item LICENSE $stage
    $zip = Join-Path $out "OneDriveHelper-$v-$($a.Name).zip"
    Remove-Item $zip -Force -ErrorAction SilentlyContinue
    Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $zip
    Remove-Item $stage -Recurse -Force
    $zips += Get-Item $zip
}

$sums = Join-Path $out "SHA256SUMS.txt"
$zips | ForEach-Object { "{0}  {1}" -f (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name } |
    Set-Content $sums -Encoding ascii

Write-Host ""
Write-Host "Release ${v}:" -ForegroundColor Green
Get-Content $sums | ForEach-Object { Write-Host "  $_" }
Write-Host ""
Write-Host "For the README's RMM install script: `$Version = '$v' and the upper-case hashes above."
