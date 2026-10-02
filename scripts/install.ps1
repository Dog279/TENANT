# install.ps1 - download the Tenant release for this CPU, verify its SHA-256
# against the release's checksums.txt, and install tenant.exe for the current
# user (no administrator rights). The release workflow stamps the repository
# name in; the tracked copy carries the __REPO__ placeholder.
#
#   irm https://github.com/OWNER/REPO/releases/latest/download/install.ps1 | iex
#
# Environment overrides:
#   TENANT_VERSION      version to install, without the leading v (default: latest release)
#   TENANT_REPO         GitHub owner/name asked for the latest release
#   TENANT_BASE_URL     directory URL that holds the archive and checksums.txt
#   TENANT_INSTALL_DIR  where tenant.exe goes (default: %LOCALAPPDATA%\Programs\tenant)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = if ($env:TENANT_REPO) { $env:TENANT_REPO } else { '__REPO__' }
$Dir = if ($env:TENANT_INSTALL_DIR) { $env:TENANT_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\tenant' }
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'x86_64' }
  'ARM64' { 'aarch64' }
  default { throw "install.ps1: unsupported CPU $env:PROCESSOR_ARCHITECTURE" }
}

$Version = $env:TENANT_VERSION
if (-not $Version) {
  $Api = "https://api.github.com/repos/$Repo/releases/latest"
  $Version = (Invoke-RestMethod -UseBasicParsing $Api).tag_name -replace '^v', ''
}
$Base = if ($env:TENANT_BASE_URL) { $env:TENANT_BASE_URL } else { "https://github.com/$Repo/releases/download/v$Version" }
$Archive = "tenant_${Version}_windows_${Arch}.zip"

$Tmp = Join-Path ([IO.Path]::GetTempPath()) "tenant-install-$PID"
New-Item -ItemType Directory -Force $Tmp | Out-Null
try {
  Write-Host "Downloading $Base/$Archive"
  Invoke-WebRequest -UseBasicParsing "$Base/$Archive" -OutFile (Join-Path $Tmp $Archive)
  Invoke-WebRequest -UseBasicParsing "$Base/checksums.txt" -OutFile (Join-Path $Tmp 'checksums.txt')

  $Pattern = '^([0-9a-f]{64})\s+' + [regex]::Escape($Archive) + '$'
  $Entry = Get-Content (Join-Path $Tmp 'checksums.txt') | Where-Object { $_ -match $Pattern } | Select-Object -First 1
  if (-not $Entry) { throw "install.ps1: $Archive is not listed in checksums.txt" }
  $Want = ($Entry -split '\s+')[0].ToLower()
  $Got = (Get-FileHash -Algorithm SHA256 (Join-Path $Tmp $Archive)).Hash.ToLower()
  if ($Got -ne $Want) { throw "install.ps1: checksum mismatch for ${Archive}: got $Got, want $Want" }

  Expand-Archive -Force (Join-Path $Tmp $Archive) -DestinationPath $Tmp
  New-Item -ItemType Directory -Force $Dir | Out-Null
  Move-Item -Force (Join-Path $Tmp 'tenant.exe') (Join-Path $Dir 'tenant.exe')
} finally {
  Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}

$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($UserPath -split ';') -notcontains $Dir) {
  [Environment]::SetEnvironmentVariable('Path', "$UserPath;$Dir".Trim(';'), 'User')
  Write-Host "Added $Dir to your user PATH; open a new terminal to pick it up."
}
if (($env:Path -split ';') -notcontains $Dir) { $env:Path = "$env:Path;$Dir" }

Write-Host "Installed $Dir\tenant.exe"
& (Join-Path $Dir 'tenant.exe') version
Write-Host 'Next: tenant setup'
