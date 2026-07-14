# Prompt-router failure drills

These drills verify the router's failure doctrine: the classifier is optional.
If it is down, malformed, too slow, or inappropriate for a large input, the
turn still follows a code-owned safe route. A classifier failure must never
become a turn failure or authorize an action.

Run the drills against an isolated data directory. Failed decisions are real
telemetry and would otherwise make a production graduation report fail.

## One-time drill setup (Windows PowerShell)

From the Tenant repository:

```powershell
$env:CGO_ENABLED = "0"
go build -o tenant.exe ./cmd/tenant

$DrillRoot = Join-Path ([IO.Path]::GetTempPath()) ("tenant-route-drills-" + [guid]::NewGuid())
$ConfigDir = Join-Path $DrillRoot "config"
$DataDir = Join-Path $DrillRoot "data"
$DecisionLog = Join-Path $DataDir "route-decisions.jsonl"
$RouterModel = "hf.co/SupraLabs/Supra-Router-51M-gguf:Q4_K_M"
New-Item -ItemType Directory -Force -Path $ConfigDir, $DataDir | Out-Null

@'
{
  "schema_version": 2,
  "provider": "small",
  "providers": {
    "small": { "kind": "echo" },
    "big": { "kind": "echo" }
  },
  "route": {
    "enabled": true,
    "mode": "active",
    "assist": false,
    "provider": {
      "endpoint": "http://127.0.0.1:11435",
      "model": "hf.co/SupraLabs/Supra-Router-51M-gguf:Q4_K_M"
    },
    "timeout_ms": 250,
    "never_cloud": true,
    "min_complexity_for_big": 4,
    "sticky_session": true,
    "big_role": "big"
  }
}
'@ | Set-Content -Encoding ASCII (Join-Path $ConfigDir "config.json")
```

Both generation tiers use Tenant's deterministic Echo backend. The classifier
still uses its real Ollama-native HTTP protocol. This isolates routing failure
from generation failure and makes repeated response text comparable.

Start a dedicated Ollama instance in terminal A. Do not use the primary model
runtime's port for a kill drill.

```powershell
$env:OLLAMA_HOST = "127.0.0.1:11435"
ollama serve
```

In terminal B, pull and warm the classifier, then validate the route config:

```powershell
$env:OLLAMA_HOST = "127.0.0.1:11435"
$RouterModel = "hf.co/SupraLabs/Supra-Router-51M-gguf:Q4_K_M"
ollama pull $RouterModel
$body = @{
  model = $RouterModel
  prompt = "Task: Say hello.`nAnalysis: "
  raw = $true
  stream = $false
  keep_alive = "5m"
} | ConvertTo-Json -Compress
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:11435/api/generate" -ContentType "application/json" -Body $body
.\tenant.exe route status --config $ConfigDir --data $DataDir
```

The status must say `classifier: healthy (parseable; ...)`. Start the isolated
TUI when a drill calls for it:

```powershell
.\tenant.exe tui --config $ConfigDir --data $DataDir --allow-no-memory --self-improve=false
```

Use a fresh TUI process for each drill. That resets the in-memory circuit
breaker while retaining the drill's JSONL evidence.

## Common verification

After each injected failure, run these commands in the TUI:

```text
/route why
/route stats
```

Wait for the `route:` activity-feed line before running `/route why`; shadow
and failure observations are deliberately asynchronous.

`/route why` must show a safe selected tier and a policy reason; parser failures
also show the exact rejected raw completion. `/route stats` must count the
decision and report the graduation criteria as PASS/FAIL with actual values.
A drill is expected to make at least one criterion fail; that is evidence that
missing or bad data is not silently skipped.

The same aggregate is available outside the TUI:

```powershell
.\tenant.exe route stats --config $ConfigDir --data $DataDir
```

Inspect the last durable decision without exposing the prompt (the log stores
only its SHA-256 hash):

```powershell
Get-Content -LiteralPath $DecisionLog -Tail 1 |
  ConvertFrom-Json |
  Format-List ts,mode,status,error,raw,classifier_attempted,latency_ms,selected_tier,policy_reason,breaker_state,breaker_tripped
```

## Drill 1: kill the auxiliary runtime mid-session

1. Start Ollama and the TUI as described above.
2. In the TUI, send `failure doctrine byte identity probe` and retain the
   assistant response.
3. In terminal B, kill only the process listening on the dedicated aux port:

   ```powershell
   $AuxPID = Get-NetTCPConnection -State Listen -LocalPort 11435 |
     Select-Object -First 1 -ExpandProperty OwningProcess
   Get-Process -Id $AuxPID
   Stop-Process -Id $AuxPID -Force
   ```

4. In the same TUI session, send `failure doctrine byte identity probe` again.
5. Run `/route why`, `/route stats`, and the durable-log command above.

Assert:

- The second turn completes and its assistant response is byte-identical to
  the first response. Echo makes this comparison deterministic.
- The failed decision has `status: error`, `selected_tier: small`, and
  `policy_reason: classifier_failure`; it does not use a stale big result.
- No tool or approval action is created by the classifier failure.
- Repeating the failed prompt three times trips the breaker. The next decision
  is skipped with `policy_reason: breaker_open`, while turns still complete.
- The graduation row for route-attributable turn failures remains zero.

Automated counterparts:

```powershell
go test ./internal/route/... -run 'Test(SupraEndpointDown|EngineBreakerTripsAndSkipsDeadClassifier)$' -count=1 -v
go test ./cmd/tenant/... -run 'Test(RouteDefaultSafe|RouteRuntimeClassifierDeadlineSelectsFastAndFallsBackSlow)$' -count=1 -v
```

Restart the dedicated Ollama process before another healthy probe.

## Fault endpoint for parser and deadline drills

For the next two drills, stop Ollama on port 11435 and paste this in terminal A.
It is an in-memory HTTP fault endpoint; it creates no script or repository file.
Set `$RouteResponse` and `$DelayMS` as directed by the drill.

```powershell
$Port = 11435
if ($null -eq $DelayMS) { $DelayMS = 0 }
if ([string]::IsNullOrWhiteSpace($RouteResponse)) { $RouteResponse = "Route: probably big" }
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Port)
$listener.Start()
try {
  while ($true) {
    $client = $listener.AcceptTcpClient()
    try {
      $stream = $client.GetStream()
      $reader = [IO.StreamReader]::new($stream, [Text.Encoding]::ASCII, $false, 1024, $true)
      $contentLength = 0
      while (($line = $reader.ReadLine()) -ne $null -and $line -ne "") {
        if ($line -match '^Content-Length:\s*(\d+)$') { $contentLength = [int]$Matches[1] }
      }
      if ($contentLength -gt 0) {
        $requestBody = New-Object char[] $contentLength
        $offset = 0
        while ($offset -lt $contentLength) {
          $n = $reader.Read($requestBody, $offset, $contentLength - $offset)
          if ($n -le 0) { break }
          $offset += $n
        }
      }
      if ($DelayMS -gt 0) { Start-Sleep -Milliseconds $DelayMS }
      $payload = @{ response = $RouteResponse; done = $true } | ConvertTo-Json -Compress
      $bodyBytes = [Text.Encoding]::UTF8.GetBytes($payload)
      $headers = "HTTP/1.1 200 OK`r`nContent-Type: application/json`r`nContent-Length: $($bodyBytes.Length)`r`nConnection: close`r`n`r`n"
      $headerBytes = [Text.Encoding]::ASCII.GetBytes($headers)
      $stream.Write($headerBytes, 0, $headerBytes.Length)
      $stream.Write($bodyBytes, 0, $bodyBytes.Length)
      $stream.Flush()
    } catch {
      Write-Host "client closed before the injected reply: $($_.Exception.Message)"
    } finally {
      $client.Close()
    }
  }
} finally {
  $listener.Stop()
}
```

Stop the endpoint with Ctrl-C before changing its variables or restarting it.
Set the drill-specific variables in that same terminal before pasting the
endpoint block again.

## Drill 2: return a strict-parser-breaking completion

Run the fault endpoint with:

```powershell
$DelayMS = 0
$RouteResponse = "Route: probably big"
```

Start a fresh TUI and send `Reply with one friendly sentence.` Then perform the
common verification.

Assert:

- The turn completes on the small/default tier.
- `/route why` contains a `route: parse:` error and the exact raw output
  `Route: probably big`.
- The log has `status: error`, `selected_tier: small`, and
  `policy_reason: classifier_failure`.
- `/route stats` increments both classifier attempts and parse failures.
- The malformed text never becomes an action or tool call. A classifier can be
  wrong only about tier; action authorization remains in Tenant policy.

Automated counterparts:

```powershell
go test ./internal/route/... -run 'Test(ParseSignalRejectsDeviations|SupraMalformedOutputPreservesRaw|EngineBreakerTripsAndSkipsDeadClassifier)$' -count=1 -v
```

## Drill 3: paste approximately 10,000 tokens

The TUI intentionally caps its textarea at 8,000 characters, so use the
line-oriented `tenant chat` harness for this oversized input. Set the global
stage to `everywhere` so this non-TUI surface exercises active routing:

```powershell
$ConfigPath = Join-Path $ConfigDir "config.json"
$cfg = Get-Content -Raw -LiteralPath $ConfigPath | ConvertFrom-Json
$cfg.route.mode = "everywhere"
$cfg.route.assist = $false
$cfg | ConvertTo-Json -Depth 20 | Set-Content -Encoding ASCII -LiteralPath $ConfigPath

$Prompt = "Summarize this deliberately oversized input: " + (" token" * 10000)
$Prompt | .\tenant.exe chat --config $ConfigDir --data $DataDir
.\tenant.exe route stats --config $ConfigDir --data $DataDir
```

Leave the classifier stopped. Inspect the last JSONL decision with the common
log command.

Assert:

- The turn completes even though the classifier endpoint is unavailable.
- `classifier_attempted` is false and `policy_reason` is `long_prompt` when the
  big target is permitted. A stronger privacy/context clamp may safely select
  small instead; the classifier is still not called.
- The input is never truncated and then misclassified as if it were the whole
  prompt.
- Classifier-attempt, parse-failure, and latency denominators do not include
  this deterministic no-classifier decision.

Automated counterpart:

```powershell
go test ./internal/route/... -run 'TestEngineDecideWithPolicyLongPromptSkipsClassifier$' -count=1 -v
```

## Drill 4: reply after the deadline

Restore `mode` to `active` in terminal B:

```powershell
$ConfigPath = Join-Path $ConfigDir "config.json"
$cfg = Get-Content -Raw -LiteralPath $ConfigPath | ConvertFrom-Json
$cfg.route.mode = "active"
$cfg.route.assist = $false
$cfg | ConvertTo-Json -Depth 20 | Set-Content -Encoding ASCII -LiteralPath $ConfigPath
```

Then, in terminal A, set a valid but late completion and paste the fault
endpoint block again:

```powershell
$DelayMS = 750
$RouteResponse = "Domain: General | Complexity: 5 | Math: False | Code: False | Route: big model | Justification: Deliberately delayed valid reply."
```

Start a fresh TUI, send `Reply with one friendly sentence.`, and perform the
common verification.

Assert:

- The turn does not wait 750 ms for routing; it continues after the 250 ms
  route deadline on the small/default tier.
- `/route why` reports a classifier deadline/timeout and
  `policy_reason: classifier_failure`.
- The valid-but-late big reply cannot mutate the already-selected tier.
- The late server write may report that the client closed. That is expected and
  must not surface as a turn error.
- The recorded classify latency is deadline-scale, not an unbounded wait.

Automated counterparts:

```powershell
go test ./internal/route/... -run 'TestSupraTimeout$' -count=1 -v
go test ./internal/agent/... -run 'TestAgentActiveRouteDeadlineJoin$' -count=1 -v
go test ./cmd/tenant/... -run 'TestRouteRuntimeClassifierDeadlineSelectsFastAndFallsBackSlow$' -count=1 -v
```

## Rollout ladder

The ladder is persisted configuration, not separate implementations. Preserve
the rest of the `route` object and change only these stage fields:

| Stage | `enabled` | `mode` | `assist` | Behavior |
|---|---:|---|---:|---|
| Disabled baseline | `false` | `shadow` | `false` | No classifier dependency; planner behavior only. |
| Shadow | `true` | `shadow` | `false` | Record decisions; never change the model used. |
| Assisted | `true` | `shadow` | `true` | Shadow plus TUI suggestions; `!big` is the operator-confirmed opt-in. |
| Active interactive | `true` | `active` | `false` | Apply routing to TUI/dashboard; unattended surfaces remain shadow unless explicitly overridden. |
| Active everywhere | `true` | `everywhere` | `false` | Apply routing to interactive and unattended surfaces. Graduate only after `/route stats` says overall PASS. |

Example stage fragments:

```json
{ "enabled": true, "mode": "shadow", "assist": false }
{ "enabled": true, "mode": "shadow", "assist": true }
{ "enabled": true, "mode": "active", "assist": false }
{ "enabled": true, "mode": "everywhere", "assist": false }
```

Configuration validation enforces the ladder:

- `assist: true` is valid only with `enabled: true` and `mode: shadow`.
- Active per-agent overrides are invalid while assist is enabled.
- `active` and `everywhere` require `big_role` to name a configured, wired
  provider.
- The classifier endpoint must remain loopback/local when routing is enabled.
- `never_cloud` and the other policy clamps remain authoritative at every
  stage, including `everywhere`.

Do not advance a stage merely because the classifier is reachable. Advance
from assisted/active only when the numeric graduation gate has enough data and
every criterion, including zero routing-attributable turn failures, passes.
