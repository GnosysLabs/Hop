[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("hop-installer-test-" + [guid]::NewGuid())
$fixtures = Join-Path $tempDir "fixtures"
$payload = Join-Path $tempDir "payload"
$installDir = Join-Path $tempDir "install"
$testArch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -eq "Arm64") { "arm64" } else { "amd64" }
$asset = "hop_windows_${testArch}.zip"

New-Item -ItemType Directory -Path $tempDir | Out-Null
New-Item -ItemType Directory -Path $fixtures, $payload | Out-Null
try {
    $binary = Join-Path $payload "hop.exe"
    & go build -trimpath `
        -ldflags "-X githop.xyz/GnosysLabs/Hop/internal/hop.Version=9.9.9-installer-test" `
        -o $binary (Join-Path $root "cmd/hop")
    if ($LASTEXITCODE -ne 0) { throw "Could not build installer test binary" }

    $archive = Join-Path $fixtures $asset
    Compress-Archive -Path $binary -DestinationPath $archive
    $hash = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
    "$hash  $asset" | Set-Content -Encoding ascii (Join-Path $fixtures "checksums.txt")

    function Invoke-RestMethod {
        [CmdletBinding()]
        param([Parameter(Mandatory)][string]$Uri)
        if (-not $Uri.EndsWith("/releases?draft=false&page=1&limit=1")) {
            throw "Unexpected installer API URL: $Uri"
        }
        return ,([pscustomobject]@{
            tag_name = "v9.9.9-installer-test"
            prerelease = $true
            draft = $false
        })
    }

    function Invoke-WebRequest {
        [CmdletBinding()]
        param(
            [switch]$UseBasicParsing,
            [Parameter(Mandatory)][string]$Uri,
            [Parameter(Mandatory)][string]$OutFile
        )
        if ($Uri.EndsWith("/checksums.txt")) {
            Copy-Item (Join-Path $fixtures "checksums.txt") $OutFile
        } elseif ($Uri.EndsWith("/$asset")) {
            Copy-Item (Join-Path $fixtures $asset) $OutFile
        } else {
            throw "Unexpected installer asset URL: $Uri"
        }
    }

    & (Join-Path $root "scripts/install.ps1") `
        -GiteaUrl "https://gitea.test" `
        -Repository "GnosysLabs/Hop" `
        -InstallDir $installDir `
        -SkipSkill `
        -SkipPath

    $installed = Join-Path $installDir "hop.exe"
    if (-not (Test-Path $installed)) { throw "Installer did not install hop.exe" }
    $version = & $installed version
    if ($LASTEXITCODE -ne 0 -or $version -ne "hop 9.9.9-installer-test") {
        throw "Unexpected installed version: $version"
    }
    Write-Host "PowerShell installer smoke test passed."
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tempDir
}
