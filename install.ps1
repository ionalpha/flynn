#Requires -Version 5
<#
.SYNOPSIS
    Flynn installer for Windows.
.DESCRIPTION
    Downloads a prebuilt release binary, verifies its SHA-256 checksum, and
    installs it. Refuses to install on a checksum mismatch.

    Usage:
        irm https://raw.githubusercontent.com/ionalpha/flynn/main/install.ps1 | iex

    Environment overrides:
        FLYNN_VERSION      install a specific tag (e.g. v0.1.0); default: the latest release
        FLYNN_INSTALL_DIR  install directory;                   default: %LOCALAPPDATA%\flynn\bin
#>
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = 'ionalpha/flynn'
$binary = 'flynn.exe'

# --- detect architecture --------------------------------------------------
if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'Flynn requires a 64-bit version of Windows.'
}
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { 'amd64' }

# --- resolve the version to install ---------------------------------------
$version = $env:FLYNN_VERSION
if (-not $version) {
    try {
        $release = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest"
        $version = $release.tag_name
    } catch {
        throw "Could not resolve the latest release; set FLYNN_VERSION (e.g. `$env:FLYNN_VERSION='v0.1.0'`). $_"
    }
}

$asset = "flynn_windows_${arch}.zip"
# FLYNN_BASE_URL overrides where the release files are fetched from (a private mirror);
# it defaults to the GitHub release for this version.
$base = $env:FLYNN_BASE_URL
if (-not $base) { $base = "https://github.com/$repo/releases/download/$version" }
$tmp = Join-Path $env:TEMP ('flynn-install-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    # --- download the archive and checksums -------------------------------
    Write-Host "Downloading $asset ($version)..."
    $archivePath = Join-Path $tmp $asset
    $sumsPath = Join-Path $tmp 'checksums.txt'
    try {
        Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $archivePath
    } catch {
        throw "Download failed: $base/$asset (does release $version include a windows/$arch build?). $_"
    }
    Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile $sumsPath

    # --- verify the SHA-256 (the mandatory integrity gate) ----------------
    $line = Get-Content $sumsPath | Where-Object { ($_ -split '\s+')[-1] -eq $asset } | Select-Object -First 1
    if (-not $line) { throw "No checksum is recorded for $asset in checksums.txt." }
    $want = (($line -split '\s+')[0]).ToLower()
    $got = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash.ToLower()
    if ($got -ne $want) {
        throw "Checksum mismatch for ${asset}: expected $want, got $got. Refusing to install."
    }
    Write-Host 'Checksum verified.'

    # --- extract ----------------------------------------------------------
    Expand-Archive -Path $archivePath -DestinationPath $tmp -Force
    $src = Join-Path $tmp $binary
    if (-not (Test-Path $src)) { throw "The archive did not contain $binary." }

    # --- install ----------------------------------------------------------
    $dir = $env:FLYNN_INSTALL_DIR
    if (-not $dir) { $dir = Join-Path $env:LOCALAPPDATA 'flynn\bin' }
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Copy-Item -Path $src -Destination (Join-Path $dir $binary) -Force

    Write-Host ''
    Write-Host "Installed $binary to $dir\$binary"

    # Add the install dir to the user PATH if it is not already there.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if (($userPath -split ';') -notcontains $dir) {
        [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $dir), 'User')
        Write-Host "Added $dir to your user PATH. Open a new terminal to pick it up."
    }
    Write-Host 'Get started:  flynn --version   then   flynn goal "..."   and   flynn spine verify <run>'
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
