[CmdletBinding()]
param(
    [string]$ApiUrl = $(if ($env:HOP_RELEASE_API_URL) { $env:HOP_RELEASE_API_URL } else { "https://api.github.com" }),
    [string]$ReleaseUrl = $(if ($env:HOP_RELEASE_URL) { $env:HOP_RELEASE_URL } else { "https://github.com" }),
    [string]$Repository = $(if ($env:HOP_REPOSITORY) { $env:HOP_REPOSITORY } else { "GnosysLabs/Hop" }),
    [string]$Version = $(if ($env:HOP_VERSION) { $env:HOP_VERSION } else { "latest" }),
    [string]$InstallDir = $(if ($env:HOP_INSTALL_DIR) { $env:HOP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\Hop" }),
    [switch]$SkipSkill,
    [switch]$SkipPath
)

$ErrorActionPreference = "Stop"
$ApiUrl = $ApiUrl.TrimEnd("/")
$ReleaseUrl = $ReleaseUrl.TrimEnd("/")

$architecture = if ($env:PROCESSOR_ARCHITEW6432) {
    [string]$env:PROCESSOR_ARCHITEW6432
} else {
    [string]$env:PROCESSOR_ARCHITECTURE
}
switch ($architecture.ToUpperInvariant()) {
    "AMD64" { $arch = "amd64" }
    "ARM64" { $arch = "arm64" }
    default { throw "Unsupported Windows architecture: $architecture" }
}

$asset = "hop_windows_${arch}.zip"
if ($Version -eq "latest") {
    $releaseResponse = Invoke-WebRequest -UseBasicParsing -Uri "$ApiUrl/repos/$Repository/releases/latest"
    if (-not $releaseResponse -or [string]::IsNullOrWhiteSpace($releaseResponse.Content)) {
        throw "Could not determine the latest published release"
    }
    $latestRelease = $releaseResponse.Content | ConvertFrom-Json
    if ($null -eq $latestRelease) { throw "Could not determine the latest published release" }
    $tag = [string]$latestRelease.tag_name
    if ([string]::IsNullOrWhiteSpace($tag)) { throw "Could not determine the latest published release" }
} else {
    $tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
}
if ($tag -notmatch '^[A-Za-z0-9._-]+$') {
    throw "Release API returned an unsafe tag: $tag"
}
$releaseUrl = "$ReleaseUrl/$Repository/releases/download/$tag"

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
        if ($LASTEXITCODE -ne 0) {
            throw "Hop skill installation failed with exit code $LASTEXITCODE"
        }
    }
    Write-Host "Installed $(& $installedBinary version)"
    Write-Host "Binary: $installedBinary"
    if (-not $SkipSkill) {
        Write-Host "Restart any open agent application, then use it normally in any Git repository."
    }
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tempDir
}


