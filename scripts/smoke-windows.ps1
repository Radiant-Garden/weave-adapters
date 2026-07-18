<#
.SYNOPSIS
  Runs the built adapter on this host and verifies it actually serves.

.DESCRIPTION
  This is the automated form of the M1 sign-off criterion ("runs on Windows
  Server 2022 and answers GET /api/v1/health"). It exercises the real binary
  against the real OS network stack rather than an httptest server:

    1. mints a bearer token through the token CLI
    2. starts the adapter on an unprivileged port
    3. asserts health answers 200 with no credentials, as weave polls it
    4. asserts a non-exempt route is 401 anonymous and 404 with the token
    5. asks the process to shut down and confirms it exits cleanly

  Step 5 sends a real console Ctrl+C, because "a console exe receives
  os.Interrupt on Windows Server 2022" is an assumption main.go states but
  nothing has ever verified. If the console attach fails the check downgrades
  to a warning rather than failing the run -- a flaky signal-delivery trick
  must not be able to redden an otherwise-good build.

.PARAMETER Exe
  Path to the adapter binary. Defaults to the build-windows output.

.PARAMETER Port
  Port to listen on. Defaults to 18444 (deliberately not the 8444 default, so
  a smoke run never collides with an adapter already running on the host).
#>
[CmdletBinding()]
param(
    [string]$Exe  = "bin\weave-adapter-dhcp-windows.exe",
    [int]   $Port = 18444
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$workDir = Join-Path $env:TEMP ("weave-smoke-" + [guid]::NewGuid().ToString("N"))
$store   = Join-Path $workDir "tokens.toml"
$stdout  = Join-Path $workDir "adapter-stdout.log"
$stderr  = Join-Path $workDir "adapter-stderr.log"
$proc    = $null

function Write-Step { param([string]$Message) Write-Host "==> $Message" }

# Invoke-WebRequest's -SkipHttpErrorCheck is PowerShell 7 only. WS2022 ships
# Windows PowerShell 5.1, where any non-2xx raises a terminating error instead
# of returning a response -- and this script needs to assert on a 401, so the
# error path is a normal outcome here, not a failure.
#
# -UseBasicParsing matters for the same reason the rest of this script is
# careful: 5.1 otherwise reaches for the Internet Explorer DOM parser, which is
# unavailable to a service account with no user profile.
function Invoke-Endpoint {
    param(
        [string]   $Uri,
        [hashtable]$Headers = @{},
        [int]      $TimeoutSec = 5
    )

    try {
        $r = Invoke-WebRequest -Uri $Uri -Headers $Headers -TimeoutSec $TimeoutSec -UseBasicParsing
        return [pscustomobject]@{ StatusCode = [int]$r.StatusCode; Content = $r.Content }
    } catch {
        $response = $_.Exception.Response
        if ($null -eq $response) {
            # No response at all -- refused, reset or timed out. That is a real
            # failure, not an HTTP status, so let it propagate.
            throw
        }

        $status = [int]$response.StatusCode
        $body   = ""
        try {
            $stream = $response.GetResponseStream()
            $reader = New-Object IO.StreamReader($stream)
            $body   = $reader.ReadToEnd()
            $reader.Close()
        } catch {
            # A status with an unreadable body is still a usable assertion.
        }

        return [pscustomobject]@{ StatusCode = $status; Content = $body }
    }
}

function Stop-Adapter {
    if ($null -ne $proc -and -not $proc.HasExited) {
        Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
    }
}

try {
    if (-not (Test-Path $Exe)) {
        throw "adapter binary not found at '$Exe' -- run 'task build-windows' first"
    }

    New-Item -ItemType Directory -Path $workDir -Force | Out-Null
    Write-Step "workspace $workDir"

    # --- 0. preflight ---------------------------------------------------------
    # The CI runner is persistent, not ephemeral, so nothing guarantees a clean
    # box. A cancelled or hard-killed job leaves an adapter running and holding
    # the port; without this, the next run fails on a bind error that reads like
    # a code defect rather than leaked state.
    $exeName = [IO.Path]::GetFileNameWithoutExtension($Exe)
    $stale   = @(Get-Process -Name $exeName -ErrorAction SilentlyContinue)
    if ($stale.Count -gt 0) {
        Write-Warning "stopping $($stale.Count) adapter process(es) left behind by an earlier run"
        $stale | Stop-Process -Force
        Start-Sleep -Seconds 1
    }

    $holder = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    if ($null -ne $holder) {
        throw "port $Port is already bound by PID $($holder.OwningProcess) -- leaked state from an earlier run, or something else on the host uses it"
    }

    # --- 1. mint a token ------------------------------------------------------
    # The CLI prints the token exactly once, on a 'Bearer <token>' line intended
    # for pasting into weave. That line is the unambiguous one to parse: the
    # bare-token line above it is only indentation-distinguished.
    Write-Step "minting a bearer token"
    $genOutput = & $Exe token gen --label ci-smoke --file $store 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "token gen failed with exit code ${LASTEXITCODE}:`n$genOutput"
    }

    $match = $genOutput | Select-String -Pattern '^\s*Bearer\s+(\S+)\s*$'
    if ($null -eq $match) {
        throw "could not parse a token out of the token gen output:`n$genOutput"
    }
    $token = $match.Matches[0].Groups[1].Value

    # --- 2. start the adapter -------------------------------------------------
    Write-Step "starting the adapter on port $Port"
    $env:WEAVE_ADAPTER_PORT              = $Port
    $env:WEAVE_ADAPTER_AUTH_TOKENS_FILE  = $store
    $env:WEAVE_ADAPTER_LOG_SEVERITY      = "debug"

    $proc = Start-Process -FilePath $Exe -PassThru `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr `
        -WindowStyle Hidden

    $health = "http://127.0.0.1:$Port/api/v1/health"
    $ready  = $false
    foreach ($attempt in 1..30) {
        if ($proc.HasExited) {
            throw "adapter exited during startup (code $($proc.ExitCode)):`n$(Get-Content $stderr -Raw)"
        }
        try {
            Invoke-Endpoint -Uri $health -TimeoutSec 2 | Out-Null
            $ready = $true
            break
        } catch {
            Start-Sleep -Milliseconds 500
        }
    }
    if (-not $ready) {
        throw "adapter did not accept connections on $Port within 15s"
    }

    # --- 3. health answers without credentials --------------------------------
    # Open by contract: weave polls health to decide whether the adapter is
    # reachable at all, so an auth failure there would read as an outage. See
    # httpserver.Unauthenticated. This is also the M1 sign-off criterion
    # verbatim -- curl /api/v1/health on a real WS2022 host.
    Write-Step "asserting health answers without credentials"
    $resp = Invoke-Endpoint -Uri $health -TimeoutSec 5

    if ($resp.StatusCode -ne 200) {
        throw "expected 200 from $health without a token, got $($resp.StatusCode): $($resp.Content)"
    }

    $body = $resp.Content | ConvertFrom-Json

    if ($body.status -ne "healthy") {
        throw "expected overall status 'healthy', got '$($body.status)': $($resp.Content)"
    }
    if (-not ($body.components.name -contains "core")) {
        throw "expected a 'core' component in the payload: $($resp.Content)"
    }
    if ($null -eq $body.version -or $body.version -eq "") {
        throw "expected a non-empty version -- check that -ldflags reached the build: $($resp.Content)"
    }

    Write-Host "    status=$($body.status) version=$($body.version) uptime=$($body.uptimeSeconds)s"

    # --- 4. everything else authenticates -------------------------------------
    # Health being open says nothing about whether auth is wired at all, so this
    # drives a route that is not exempt. An unmatched path is the honest choice:
    # httpserver.Unauthenticated documents that paths matching no route still
    # authenticate, so an anonymous caller cannot enumerate what exists.
    #
    # The pair matters more than either half. 401 anonymous proves the
    # middleware is engaged; 404 with the token proves the token actually
    # authenticated and reached the router, rather than the 401 having come
    # from something unrelated.
    $guarded = "http://127.0.0.1:$Port/api/v1/does-not-exist"

    Write-Step "asserting a non-exempt route rejects an anonymous caller"
    $anon = Invoke-Endpoint -Uri $guarded -TimeoutSec 5
    if ($anon.StatusCode -ne 401) {
        throw "expected 401 from $guarded without a token, got $($anon.StatusCode): $($anon.Content)"
    }

    Write-Step "asserting the minted token authenticates"
    $authed = Invoke-Endpoint -Uri $guarded -TimeoutSec 5 `
        -Headers @{ Authorization = "Bearer $token" }
    if ($authed.StatusCode -ne 404) {
        throw "expected 404 from $guarded with a valid token (past auth, no such route), got $($authed.StatusCode): $($authed.Content)"
    }

    # --- 5. graceful shutdown -------------------------------------------------
    # Attach to the child's console and raise Ctrl+C there. SetConsoleCtrlHandler
    # with a null handler and Add=$true first, so this script does not kill
    # itself with the event it is about to generate.
    Write-Step "asserting the process shuts down gracefully on Ctrl+C"
    $gracefulChecked = $false

    if (-not ("WeaveConsole" -as [type])) {
        Add-Type -Name WeaveConsole -Namespace Win32 -MemberDefinition @'
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool AttachConsole(uint dwProcessId);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool SetConsoleCtrlHandler(IntPtr handler, bool add);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool GenerateConsoleCtrlEvent(uint dwCtrlEvent, uint dwProcessGroupId);
'@
    }

    [Win32.WeaveConsole]::FreeConsole() | Out-Null
    if ([Win32.WeaveConsole]::AttachConsole([uint32]$proc.Id)) {
        [Win32.WeaveConsole]::SetConsoleCtrlHandler([IntPtr]::Zero, $true) | Out-Null
        [Win32.WeaveConsole]::GenerateConsoleCtrlEvent(0, 0) | Out-Null
        [Win32.WeaveConsole]::FreeConsole() | Out-Null
        $gracefulChecked = $true
    } else {
        # Expected under CI: the runner service executes jobs as
        # NT AUTHORITY\NetworkService in a non-interactive session with no
        # console to attach to. This check therefore only yields a real signal
        # when run interactively on the host. It stays a warning rather than a
        # failure so that CI does not depend on a console trick.
        Write-Warning "no console to attach to (expected when running as a service account); graceful-shutdown check skipped"
    }

    if ($gracefulChecked) {
        if (-not $proc.WaitForExit(10000)) {
            throw "adapter did not exit within 10s of Ctrl+C -- graceful shutdown is hung"
        }
        if ($proc.ExitCode -ne 0) {
            throw "adapter exited with code $($proc.ExitCode) after Ctrl+C, expected 0:`n$(Get-Content $stderr -Raw)"
        }
        Write-Host "    exited cleanly (code 0)"
    }

    Write-Step "smoke test passed"
}
catch {
    Write-Host "--- adapter stdout ---"
    if (Test-Path $stdout) { Get-Content $stdout }
    Write-Host "--- adapter stderr ---"
    if (Test-Path $stderr) { Get-Content $stderr }
    throw
}
finally {
    Stop-Adapter
    Remove-Item env:WEAVE_ADAPTER_PORT             -ErrorAction SilentlyContinue
    Remove-Item env:WEAVE_ADAPTER_AUTH_TOKENS_FILE -ErrorAction SilentlyContinue
    Remove-Item env:WEAVE_ADAPTER_LOG_SEVERITY     -ErrorAction SilentlyContinue
    # The store holds a hash, not the token, but it is still scratch state.
    Remove-Item $workDir -Recurse -Force -ErrorAction SilentlyContinue
}
