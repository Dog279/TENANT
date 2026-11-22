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
#
# To make this machine a child of a fleet in the same paste, set TENANT_CHILD:
# the script goes straight on to `tenant child install`.
#
#   $env:TENANT_CHILD = 'user'; $env:TENANT_ENROLL_CODE = '<code>'; irm .../install.ps1 | iex
#
# (`tenant fleet enroll` prints this line with the code in, for 'user' and
# for 'system'.)
#
#   TENANT_CHILD        user: the child runs as this account, from logon. system: as
#                       SYSTEM from boot (run this from an elevated PowerShell;
#                       tenant.exe then goes to %ProgramFiles%\Tenant, where only
#                       administrators can replace it)
#   TENANT_ENROLL_CODE  the one-time code `tenant fleet enroll <name>` printed on the
#                       master. Left unset, it is asked for. It is never passed as an
#                       argument, where the process list shows it.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = if ($env:TENANT_REPO) { $env:TENANT_REPO } else { '__REPO__' }
$Child = $env:TENANT_CHILD
if ($Child -and $Child -ne 'user' -and $Child -ne 'system') { throw "install.ps1: TENANT_CHILD is user or system, not $Child" }
if ($Child -eq 'system') {
  $Me = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
  if (-not $Me.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'install.ps1: TENANT_CHILD=system installs a service that runs as SYSTEM: run this from an elevated PowerShell'
  }
}
$Dir = Join-Path $env:LOCALAPPDATA 'Programs\tenant'
if ($Child -eq 'system') { $Dir = Join-Path $env:ProgramFiles 'Tenant' }
if ($env:TENANT_INSTALL_DIR) { $Dir = $env:TENANT_INSTALL_DIR }
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
if ($Child) {
  # The code is read from $env:TENANT_ENROLL_CODE, or tenant asks for it.
  $ChildArgs = @('child', 'install')
  if ($Child -eq 'system') { $ChildArgs += '--system' }
  try {
    & (Join-Path $Dir 'tenant.exe') @ChildArgs
    if ($LASTEXITCODE -ne 0) { throw "install.ps1: tenant child install failed (exit $LASTEXITCODE)" }
  } finally {
    Remove-Item Env:TENANT_ENROLL_CODE -ErrorAction SilentlyContinue # not left in this session
  }
} else {
  Write-Host 'Next: tenant setup'
}
