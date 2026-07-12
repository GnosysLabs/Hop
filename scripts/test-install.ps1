[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("hop-installer-test-" + [guid]::NewGuid())
$fixtures = Join-Path $tempDir "fixtures"
$payload = Join-Path $tempDir "payload"
$installDir = Join-Path $tempDir "install"
$testHome = Join-Path $tempDir "home"
$testCodexHome = Join-Path $testHome ".codex"
$testArch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() -eq "Arm64") { "arm64" } else { "amd64" }
$asset = "hop_windows_${testArch}.zip"
$originalHome = $env:HOME
$originalUserProfile = $env:USERPROFILE
$originalCodexHome = $env:CODEX_HOME

New-Item -ItemType Directory -Path $tempDir | Out-Null
New-Item -ItemType Directory -Path $fixtures, $payload, $testHome | Out-Null
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

    $env:HOME = $testHome
    $env:USERPROFILE = $testHome
    $env:CODEX_HOME = $testCodexHome

    & (Join-Path $root "scripts/install.ps1") `
        -GiteaUrl "https://gitea.test" `
        -Repository "GnosysLabs/Hop" `
        -InstallDir $installDir `
        -SkipPath

    $installed = Join-Path $installDir "hop.exe"
    if (-not (Test-Path $installed)) { throw "Installer did not install hop.exe" }
    $version = & $installed version
    if ($LASTEXITCODE -ne 0 -or $version -ne "hop 9.9.9-installer-test") {
        throw "Unexpected installed version: $version"
    }

    $sharedBundle = Join-Path $testHome ".agents\skills\hop"
    $codexBundle = Join-Path $testCodexHome "skills\hop"
    $claudeBundle = Join-Path $testHome ".claude\skills\hop"
    $sharedSkill = Join-Path $sharedBundle "SKILL.md"
    $codexSkill = Join-Path $codexBundle "SKILL.md"
    $claudeSkill = Join-Path $claudeBundle "SKILL.md"
    if (-not (Test-Path -LiteralPath $sharedSkill -PathType Leaf) -or (Get-Item -LiteralPath $sharedSkill).Length -eq 0) {
        throw "Installer did not install the shared Hop skill"
    }
    if (-not (Test-Path -LiteralPath $codexSkill -PathType Leaf) -or (Get-Item -LiteralPath $codexSkill).Length -eq 0) {
        throw "Installer did not install the Codex Hop skill"
    }
    if (-not (Test-Path -LiteralPath $claudeSkill -PathType Leaf) -or (Get-Item -LiteralPath $claudeSkill).Length -eq 0) {
        throw "Installer did not install the Claude Code Hop skill"
    }

    function Get-BundleHashes {
        param([Parameter(Mandatory)][string]$BundlePath)
        $hashes = @{}
        Get-ChildItem -LiteralPath $BundlePath -File -Recurse | ForEach-Object {
            $relative = $_.FullName.Substring($BundlePath.Length).TrimStart(
                [System.IO.Path]::DirectorySeparatorChar,
                [System.IO.Path]::AltDirectorySeparatorChar
            )
            $hashes[$relative] = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash
        }
        return $hashes
    }

    $sharedHashes = Get-BundleHashes $sharedBundle
    $codexHashes = Get-BundleHashes $codexBundle
    $claudeHashes = Get-BundleHashes $claudeBundle
    $sharedFiles = @($sharedHashes.Keys | Sort-Object)
    $codexFiles = @($codexHashes.Keys | Sort-Object)
    $claudeFiles = @($claudeHashes.Keys | Sort-Object)
    if ($sharedFiles.Count -eq 0 -or
        (Compare-Object -ReferenceObject $sharedFiles -DifferenceObject $codexFiles) -or
        (Compare-Object -ReferenceObject $sharedFiles -DifferenceObject $claudeFiles)) {
        throw "Installed Hop skill bundles contain different files"
    }
    foreach ($relative in $sharedFiles) {
        if ($sharedHashes[$relative] -ne $codexHashes[$relative] -or
            $sharedHashes[$relative] -ne $claudeHashes[$relative]) {
            throw "Installed Hop skill bundles differ at $relative"
        }
    }

    $blockedCodexHome = Join-Path $testHome "blocked-codex"
    $blockedSkills = Join-Path $blockedCodexHome "skills"
    New-Item -ItemType Directory -Path $blockedSkills | Out-Null
    "blocked" | Set-Content -Encoding ascii (Join-Path $blockedSkills "hop")
    $env:CODEX_HOME = $blockedCodexHome
    $installFailed = $false
    try {
        & (Join-Path $root "scripts/install.ps1") `
            -GiteaUrl "https://gitea.test" `
            -Repository "GnosysLabs/Hop" `
            -InstallDir $installDir `
            -SkipPath
    } catch {
        $installFailed = $true
    }
    if (-not $installFailed) {
        throw "Installer did not fail when skill installation failed"
    }
    $env:CODEX_HOME = $testCodexHome
    Write-Host "PowerShell installer smoke test passed."
} finally {
    $env:HOME = $originalHome
    $env:USERPROFILE = $originalUserProfile
    $env:CODEX_HOME = $originalCodexHome
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tempDir
}
