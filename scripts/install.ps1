[CmdletBinding()]
param(
    [string]$GiteaUrl = $(if ($env:HOP_GITEA_URL) { $env:HOP_GITEA_URL } else { "https://githop.xyz" }),
    [string]$Repository = $(if ($env:HOP_REPOSITORY) { $env:HOP_REPOSITORY } else { "GnosysLabs/Hop" }),
    [string]$Version = $(if ($env:HOP_VERSION) { $env:HOP_VERSION } else { "latest" }),
    [string]$InstallDir = $(if ($env:HOP_INSTALL_DIR) { $env:HOP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\Hop" }),
    [switch]$SkipSkill,
    [switch]$SkipPath
)

$ErrorActionPreference = "Stop"
$GiteaUrl = $GiteaUrl.TrimEnd("/")

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    "X64" { $arch = "amd64" }
    "Arm64" { $arch = "arm64" }
    default { throw "Unsupported Windows architecture: $($_)" }
}

$asset = "hop_windows_${arch}.zip"
if ($Version -eq "latest") {
    # Gitea's /releases/latest endpoint omits prereleases. Select the newest
    # published release so the normal installer also works during Hop's alpha.
    $releases = @(Invoke-RestMethod -Uri "$GiteaUrl/api/v1/repos/$Repository/releases?draft=false&page=1&limit=1")
    $latestRelease = $releases | Select-Object -First 1
    $tag = $latestRelease.tag_name
    if (-not $tag) { throw "Could not determine the latest published release" }
} else {
    $tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
}
if ($tag -notmatch '^[A-Za-z0-9._-]+$') {
    throw "Release API returned an unsafe tag: $tag"
}
$releaseUrl = "$GiteaUrl/$Repository/releases/download/$tag"

$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("hop-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tempDir | Out-Null
try {
    $archivePath = Join-Path $tempDir $asset
    $checksumsPath = Join-Path $tempDir "checksums.txt"
    Write-Host "Downloading $asset..."
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/$asset" -OutFile $archivePath
    Invoke-WebRequest -UseBasicParsing -Uri "$releaseUrl/checksums.txt" -OutFile $checksumsPath

    $escapedAsset = [regex]::Escape($asset)
    $checksumLine = Get-Content $checksumsPath | Where-Object { $_ -match "^([a-fA-F0-9]{64})\s+\*?$escapedAsset$" } | Select-Object -First 1
    if (-not $checksumLine) { throw "checksums.txt does not contain $asset" }
    $expected = ([regex]::Match($checksumLine, "^[a-fA-F0-9]{64}")).Value.ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 $archivePath).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $asset" }

    $extractPath = Join-Path $tempDir "archive"
    Expand-Archive -Path $archivePath -DestinationPath $extractPath
    $binary = Join-Path $extractPath "hop.exe"
    if (-not (Test-Path $binary)) { throw "Release archive does not contain hop.exe" }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $installedBinary = Join-Path $InstallDir "hop.exe"
    Copy-Item -Force $binary $installedBinary

    if (-not $SkipPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $pathEntries = @($userPath -split ";" | Where-Object { $_ })
        if ($pathEntries -notcontains $InstallDir) {
            $newUserPath = (($pathEntries + $InstallDir) -join ";")
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            Write-Host "Added $InstallDir to your user PATH."
        }
        if (($env:Path -split ";") -notcontains $InstallDir) {
            $env:Path = "$InstallDir;$env:Path"
        }
    }

    if (-not $SkipSkill) {
        & $installedBinary skill install --force
    }
    Write-Host "Installed $(& $installedBinary version)"
    Write-Host "Binary: $installedBinary"
    if (-not $SkipSkill) {
        Write-Host "Restart Codex if it is open, then use it normally in any Git repository."
    }
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tempDir
}
