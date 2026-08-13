<#
.SYNOPSIS
  Migrate the Tenant command center from this workstation to a 24/7 Windows
  server over PS-Remoting.

.DESCRIPTION
  Run ON THE WORKSTATION as the user who owns the current Tenant install.
  Prompts for server credentials (Get-Credential). Everything lands under
  C:\Tenant on the server - profile-independent paths so the scheduled tasks
  run as SYSTEM with no stored password:

    C:\Tenant\tenant.exe          the binary (copied from this repo build)
    C:\Tenant\config\             config.json, credentials.json, soul, mcp
    C:\Tenant\data\               episodes/facts/wiki/archive/relay-inbox
    C:\Tenant\claude-config\      CLAUDE_CONFIG_DIR for the claude CLI
    C:\Tenant\ollama-models\      OLLAMA_MODELS (embed model storage)

  Machine-scope env vars set on the server:
    TENANT_CLAUDECODE_WEEKLY_BUDGET   subscription budget rail
    CLAUDE_CODE_OAUTH_TOKEN           subscription auth for claude -p
                                      (from 'claude setup-token', pasted when
                                      prompted; skippable)
    TENANT_CLAUDE_BIN                 path to the server's claude.exe
    CLAUDE_CONFIG_DIR / OLLAMA_MODELS fixed paths above

  Startup tasks created (SYSTEM, ONSTART):
    OllamaServe   ollama serve                      (embeddings)
    TenantServe   tenant serve --config ... --data ...

.NOTES
  BEFORE RUNNING: 'claude setup-token' on this workstation (browser flow) and
  keep the token handy - the script prompts for it.
  AFTER VERIFYING the server relay answers a Discord DM: disable the relay on
  this workstation (config relay.enabled=false or /relay off) - two live
  gateways on one bot token answer every DM twice.
  ASCII ONLY in this file: PowerShell 5.1 reads BOM-less files as ANSI, and
  multi-byte punctuation (em dashes) decodes into curly quotes that break the
  parser mid-string.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)] [string]$Server,
    [string]$RemoteRoot = "C:\Tenant",
    [long]$WeeklyBudget = 5000000
)

$ErrorActionPreference = "Stop"

# --- local sources (run as the owning user: real profile dirs, no MSIX shadow)
$localCfg = Join-Path $env:APPDATA "tenant"
$localDat = Join-Path $env:LOCALAPPDATA "tenant"
$repoExe  = Join-Path (Split-Path -Parent $PSScriptRoot) "tenant.exe"

foreach ($p in @($localCfg, $localDat, $repoExe)) {
    if (-not (Test-Path $p)) { throw "missing local source: $p" }
}

Write-Host "== Tenant migration to $Server ==" -ForegroundColor Cyan
$cred = Get-Credential -Message "Credentials for $Server"
$sess = New-PSSession -ComputerName $Server -Credential $cred

try {
    # --- preflight -------------------------------------------------------
    $pre = Invoke-Command -Session $sess -ScriptBlock {
        $dgx = Test-NetConnection -ComputerName 192.168.1.229 -Port 8000 -WarningAction SilentlyContinue
        [pscustomobject]@{
            OS        = (Get-CimInstance Win32_OperatingSystem).Caption
            PS        = $PSVersionTable.PSVersion.ToString()
            DGXReach  = $dgx.TcpTestSucceeded
            HasOllama = [bool](Get-Command ollama -ErrorAction SilentlyContinue)
        }
    }
    Write-Host ("server: {0} (PS {1})" -f $pre.OS, $pre.PS)
    if (-not $pre.DGXReach) {
        Write-Warning "server cannot reach the DGX at 192.168.1.229:8000 - the local-model leg will be dead until that routes; continuing (claudecode/zai legs still work)."
    }

    # --- layout + payload -------------------------------------------------
    Write-Host "staging $RemoteRoot ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        param($root)
        foreach ($d in @($root, "$root\config", "$root\data", "$root\claude-config", "$root\ollama-models")) {
            New-Item -ItemType Directory -Force -Path $d | Out-Null
        }
    } -ArgumentList $RemoteRoot

    Write-Host "copying binary + config + data ..." -ForegroundColor Cyan
    Copy-Item $repoExe -Destination "$RemoteRoot\tenant.exe" -ToSession $sess -Force
    Copy-Item "$localCfg\*" -Destination "$RemoteRoot\config" -ToSession $sess -Recurse -Force
    Copy-Item "$localDat\*" -Destination "$RemoteRoot\data" -ToSession $sess -Recurse -Force

    # --- claude CLI (subscription escalation leg) -------------------------
    Write-Host "installing claude CLI on server ..." -ForegroundColor Cyan
    $claudeBin = Invoke-Command -Session $sess -ScriptBlock {
        try {
            Invoke-RestMethod https://claude.ai/install.ps1 | Invoke-Expression | Out-Null
        } catch { }
        $candidates = @(
            (Join-Path $env:USERPROFILE ".local\bin\claude.exe"),
            "C:\Program Files\claude\claude.exe"
        )
        foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
        return ""
    }
    if (-not $claudeBin) {
        Write-Warning "claude CLI install could not be verified - the claudecode leg will fail over to zai/DGX until you install it on the server and set TENANT_CLAUDE_BIN."
    } else {
        Write-Host "claude CLI: $claudeBin"
    }

    # PLAIN Read-Host on purpose: the masked -AsSecureString prompt silently
    # truncates pasted input to a single character (observed live: a 1-char
    # token stored -> API 400s). The token shows on the operator's console;
    # length is echoed so a bad paste is caught before it ships.
    $tokenPlain = Read-Host "paste the 'claude setup-token' output (Enter to skip claudecode auth for now)"
    if ($tokenPlain) { Write-Host ("token length: {0}" -f $tokenPlain.Length) }

    # --- machine env ------------------------------------------------------
    Write-Host "setting machine environment ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        param($root, $budget, $bin, $tok)
        [Environment]::SetEnvironmentVariable("TENANT_CLAUDECODE_WEEKLY_BUDGET", "$budget", "Machine")
        [Environment]::SetEnvironmentVariable("CLAUDE_CONFIG_DIR", "$root\claude-config", "Machine")
        [Environment]::SetEnvironmentVariable("OLLAMA_MODELS", "$root\ollama-models", "Machine")
        if ($bin) { [Environment]::SetEnvironmentVariable("TENANT_CLAUDE_BIN", $bin, "Machine") }
        if ($tok) { [Environment]::SetEnvironmentVariable("CLAUDE_CODE_OAUTH_TOKEN", $tok, "Machine") }
    } -ArgumentList $RemoteRoot, $WeeklyBudget, $claudeBin, $tokenPlain
    $tokenPlain = $null

    # --- ollama (embeddings) ---------------------------------------------
    Write-Host "ensuring ollama + embed model ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        param($root)
        # Try winget first (ships with Server 2025), but a fresh server over a
        # remote session is exactly where winget is least dependable - so fall
        # back to the standalone release zip into $root\ollama. The zip is the
        # better fit for a SYSTEM service anyway: no installer, no per-user
        # autostart cruft.
        # resolveOllama checks every known install location EXPLICITLY - a
        # fresh winget install is NOT on this session's PATH (PATH was
        # captured at session start), so Get-Command alone lies here.
        function Resolve-Ollama {
            param($root)
            $found = Get-Command ollama -ErrorAction SilentlyContinue
            if ($found) { return $found.Source }
            foreach ($cand in @(
                (Join-Path $env:LOCALAPPDATA "Programs\Ollama\ollama.exe"),
                "C:\Program Files\Ollama\ollama.exe",
                "$root\ollama\ollama.exe"
            )) {
                if (Test-Path $cand) { return $cand }
            }
            return ""
        }

        $ol = Resolve-Ollama $root
        if (-not $ol) {
            if (Get-Command winget -ErrorAction SilentlyContinue) {
                try {
                    winget install --id Ollama.Ollama --accept-source-agreements --accept-package-agreements --silent
                } catch { }
            }
            $ol = Resolve-Ollama $root
        }
        if (-not $ol) {
            Write-Host "winget path unavailable - downloading standalone ollama zip ..."
            $ProgressPreference = "SilentlyContinue"
            $zip = "$root\ollama.zip"
            [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
            Invoke-WebRequest -Uri "https://github.com/ollama/ollama/releases/latest/download/ollama-windows-amd64.zip" -OutFile $zip -UseBasicParsing
            Expand-Archive -Path $zip -DestinationPath "$root\ollama" -Force
            Remove-Item $zip -Force
            $ol = Resolve-Ollama $root
        }
        if (-not $ol) {
            Write-Warning "ollama not found after install attempts - embeddings will be down (tenant refuses serve without --allow-no-memory)"
            return
        }
        Write-Host "ollama: $ol"

        # Embed model comes from the migrated config so the pull matches reality.
        $cfg = Get-Content "$root\config\config.json" -Raw | ConvertFrom-Json
        $model = $cfg.embed.model
        if (-not $model) { $model = "nomic-embed-text" }
        $env:OLLAMA_MODELS = "$root\ollama-models"
        Start-Process -FilePath $ol -ArgumentList "serve" -WindowStyle Hidden
        Start-Sleep -Seconds 3
        # ollama pull writes PROGRESS to stderr; over PS-Remoting stderr
        # becomes error records and a Stop preference kills the whole run
        # mid-pull. cmd /c merges the streams at the native level so only
        # plain stdout strings cross the wire.
        cmd /c "`"$ol`" pull $model 2>&1" | Select-Object -Last 5
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "ollama pull $model exited $LASTEXITCODE - embeddings may be incomplete; re-run the script to resume the pull"
        }
        # Startup task so embeddings survive reboots (SYSTEM, fixed model dir via machine env).
        schtasks /Create /F /TN "OllamaServe" /SC ONSTART /RU SYSTEM /TR "`"$ol`" serve" | Out-Null
    } -ArgumentList $RemoteRoot

    # --- tenant startup task ---------------------------------------------
    Write-Host "creating TenantServe startup task ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        param($root)
        $cmd = "`"$root\tenant.exe`" serve --config `"$root\config`" --data `"$root\data`""
        schtasks /Create /F /TN "TenantServe" /SC ONSTART /RU SYSTEM /TR $cmd | Out-Null
    } -ArgumentList $RemoteRoot

    # --- doctor + launch --------------------------------------------------
    Write-Host "running tenant doctor on server ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        param($root)
        # Same stderr-over-remoting guard as the pull: doctor diagnostics on
        # stderr must not abort the run.
        cmd /c "`"$root\tenant.exe`" doctor --config `"$root\config`" --data `"$root\data`" 2>&1" | Select-Object -Last 25
    } -ArgumentList $RemoteRoot

    Write-Host "starting tasks now (no reboot needed) ..." -ForegroundColor Cyan
    Invoke-Command -Session $sess -ScriptBlock {
        schtasks /Run /TN "OllamaServe" | Out-Null
        Start-Sleep -Seconds 2
        schtasks /Run /TN "TenantServe" | Out-Null
        Start-Sleep -Seconds 5
        schtasks /Query /TN "TenantServe" /V /FO LIST | Select-String "Status", "Last Result"
    }

    Write-Host ""
    Write-Host "== done. NEXT STEPS ==" -ForegroundColor Green
    Write-Host " 1. DM the Discord bot - the SERVER should answer (give the gateway ~30s to connect)."
    Write-Host " 2. THEN disable the relay on this workstation (two gateways = every DM answered twice):"
    Write-Host "      run the TUI here and use /relay off (or set relay.enabled=false in config.json)."
    Write-Host " 3. Reboot the server once to prove both tasks come back on their own."
} finally {
    Remove-PSSession $sess
}
