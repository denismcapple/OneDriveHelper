<#
.SYNOPSIS
  Builds OneDriveHelper.exe and OneDriveHelperAgent.exe for amd64 and arm64 into .\dist\<arch>.
.EXAMPLE
  .\build.ps1 -Version 1.0.0
#>
param(
    [string]$Version = "dev",
    [string[]]$Arch = @("amd64", "arm64")
)
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$pkg = "github.com/denismcapple/OneDriveHelper/internal/version"
$env:GOOS = "windows"
$env:CGO_ENABLED = "0"

go vet ./...
if ($LASTEXITCODE) { exit $LASTEXITCODE }
go test ./...
if ($LASTEXITCODE) { exit $LASTEXITCODE }

foreach ($a in $Arch) {
    $env:GOARCH = $a
    $out = Join-Path "dist" $a
    New-Item -ItemType Directory -Force $out | Out-Null
    $ld = "-s -w -X $pkg.Version=$Version"
    go build -trimpath -ldflags $ld -o "$out\OneDriveHelper.exe" ./cmd/OneDriveHelper
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
    # GUI subsystem: no console window flashes when toasts/protocol links launch the agent.
    go build -trimpath -ldflags "$ld -H=windowsgui" -o "$out\OneDriveHelperAgent.exe" ./cmd/OneDriveHelperAgent
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
    go build -trimpath -ldflags "$ld -H=windowsgui" -o "$out\OneDriveHelperConfig.exe" ./cmd/OneDriveHelperConfig
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
    Write-Host "Built $out"
}
Remove-Item Env:GOARCH
