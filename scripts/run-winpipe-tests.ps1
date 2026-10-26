param(
    [string]$PackageDir = $PSScriptRoot,
    [switch]$CheckOnly,
    [switch]$SelfTest
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$required = @(
    'winpipe.test.exe',
    'winrelay.test.exe',
    'run-winpipe-tests.ps1',
    'README.txt'
)

function Read-Manifest([string]$dir) {
    $sums = Join-Path $dir 'SHA256SUMS'
    if (-not (Test-Path -LiteralPath $sums)) {
        throw 'SHA256SUMS is missing'
    }
    $entries = @{}
    foreach ($line in Get-Content -LiteralPath $sums) {
        if ($line.Trim().Length -eq 0) {
            continue
        }
        if ($line -notmatch '^([0-9a-fA-F]{64}) {2}([^\\/]+)$') {
            throw ('malformed SHA256SUMS line: ' + $line)
        }
        $digest = $Matches[1].ToLowerInvariant()
        $name = $Matches[2]
        if ($name -eq '.' -or $name -eq '..' -or $name.Contains('..')) {
            throw ('invalid manifest name: ' + $name)
        }
        if ($entries.ContainsKey($name)) {
            throw ('duplicate manifest entry: ' + $name)
        }
        if ($required -notcontains $name) {
            throw ('unexpected manifest entry: ' + $name)
        }
        $entries[$name] = $digest
    }
    foreach ($name in $required) {
        if (-not $entries.ContainsKey($name)) {
            throw ('manifest is missing ' + $name)
        }
    }
    foreach ($name in $required) {
        $path = Join-Path $dir $name
        if (-not (Test-Path -LiteralPath $path)) {
            throw ('missing file: ' + $name)
        }
        $actual = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $entries[$name]) {
            throw ('hash mismatch: ' + $name)
        }
    }
    return $entries
}

# Invoke-OwnedProcess starts exactly one child, drains stdout/stderr into the
# supplied files while it runs, and bounds every wait. If Start or a wait
# throws, the child is still killed and joined when it was started; the result
# carries the original PID and the error. If the kill fails, the PID is reported
# and the function still returns.
function Invoke-OwnedProcess(
    [string]$exe,
    [string]$argLine,
    [int]$timeoutSec,
    [string]$workDir,
    [string]$stdoutPath,
    [string]$stderrPath) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $exe
    $psi.Arguments = $argLine
    $psi.WorkingDirectory = $workDir
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    $proc = New-Object System.Diagnostics.Process
    $proc.StartInfo = $psi

    $outFs = $null
    $errFs = $null
    $outTask = $null
    $errTask = $null
    $started = $false
    $timedOut = $false
    $killFailed = $false
    $joinTimedOut = $false
    $code = -1
    $childPid = -1
    $runError = ''
    try {
        try {
            $outFs = [System.IO.File]::Open($stdoutPath, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::Read)
            $errFs = [System.IO.File]::Open($stderrPath, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::Read)
            $proc.Start() | Out-Null
            $started = $true
            $childPid = $proc.Id
            $outTask = $proc.StandardOutput.BaseStream.CopyToAsync($outFs)
            $errTask = $proc.StandardError.BaseStream.CopyToAsync($errFs)
            if (-not $proc.WaitForExit($timeoutSec * 1000)) {
                $timedOut = $true
            }
        }
        catch {
            $runError = $_.Exception.Message
        }

        # Timeout and exception paths both reach here. Kill only the process
        # started above and join with a bound.
        if ($started) {
            try {
                if (-not $proc.HasExited) {
                    try {
                        $proc.Kill()
                    }
                    catch {
                        $killFailed = $true
                        if ($runError.Length -eq 0) {
                            $runError = 'kill: ' + $_.Exception.Message
                        }
                    }
                    if (-not $proc.WaitForExit(5000)) {
                        $joinTimedOut = $true
                    }
                }
            }
            catch {
                $joinTimedOut = $true
                if ($runError.Length -eq 0) {
                    $runError = 'join: ' + $_.Exception.Message
                }
            }
        }
        if ($null -ne $outTask -and $null -ne $errTask) {
            try {
                $tasks = [System.Threading.Tasks.Task[]]@($outTask, $errTask)
                if (-not [System.Threading.Tasks.Task]::WaitAll($tasks, 5000)) {
                    $joinTimedOut = $true
                }
            }
            catch {
                $joinTimedOut = $true
                if ($runError.Length -eq 0) {
                    $runError = 'output join: ' + $_.Exception.Message
                }
            }
        }
        if ($started) {
            try {
                if ($proc.HasExited) {
                    $code = $proc.ExitCode
                }
            }
            catch {
                $code = -1
                if ($runError.Length -eq 0) {
                    $runError = 'exit code: ' + $_.Exception.Message
                }
            }
        }
    }
    finally {
        if ($null -ne $outFs) {
            try { $outFs.Dispose() } catch { }
        }
        if ($null -ne $errFs) {
            try { $errFs.Dispose() } catch { }
        }
        try { $proc.Dispose() } catch { }
    }
    return [pscustomobject]@{
        ExitCode     = $code
        TimedOut     = $timedOut
        KillFailed   = $killFailed
        JoinTimedOut = $joinTimedOut
        Pid          = $childPid
        Error        = $runError
    }
}

function Test-Marker([string]$path, [string]$marker) {
    if (-not (Test-Path -LiteralPath $path)) {
        return $false
    }
    $text = Get-Content -LiteralPath $path -Raw -ErrorAction SilentlyContinue
    return ($null -ne $text -and $text.Contains($marker))
}

function Test-StderrMarkers([string]$stdoutPath, [string]$stderrPath, [string]$outMarker, [string]$errMarker) {
    $out = ''
    $err = ''
    if (Test-Path -LiteralPath $stdoutPath) {
        $out = Get-Content -LiteralPath $stdoutPath -Raw -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $stderrPath) {
        $err = Get-Content -LiteralPath $stderrPath -Raw -ErrorAction SilentlyContinue
    }
    return ($null -ne $out -and $out.Contains($outMarker) -and
        $null -ne $err -and $err.Contains($errMarker))
}

if ($SelfTest) {
    $shell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ('boxdwel-winpipe-selftest-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tempDir | Out-Null
    $selfExit = 0
    try {
        $outFile = Join-Path $tempDir 'exit.stdout.log'
        $errFile = Join-Path $tempDir 'exit.stderr.log'
        $exitArgs = '-NoProfile -Command "Write-Output ''stdout-ok''; [Console]::Error.WriteLine(''stderr-ok''); exit 7"'
        $exitRun = Invoke-OwnedProcess $shell $exitArgs 20 $tempDir $outFile $errFile
        $exitMarkers = Test-StderrMarkers $outFile $errFile 'stdout-ok' 'stderr-ok'

        $hangOut = Join-Path $tempDir 'hang.stdout.log'
        $hangErr = Join-Path $tempDir 'hang.stderr.log'
        $hangArgs = '-NoProfile -Command "Write-Output ''marker-out''; [Console]::Error.WriteLine(''marker-err''); Start-Sleep -Seconds 60"'
        $hangRun = Invoke-OwnedProcess $shell $hangArgs 2 $tempDir $hangOut $hangErr
        $hangMarkers = Test-StderrMarkers $hangOut $hangErr 'marker-out' 'marker-err'

        $earlyOut = Join-Path $tempDir 'early.stdout.log'
        $earlyErr = Join-Path $tempDir 'early.stderr.log'
        $earlyArgs = '-NoProfile -Command "Write-Output ''early-out''; exit 1"'
        $earlyRun = Invoke-OwnedProcess $shell $earlyArgs 20 $tempDir $earlyOut $earlyErr
        $earlyMarkers = Test-Marker $earlyOut 'early-out'

        $missingOut = Join-Path $tempDir 'missing.stdout.log'
        $missingErr = Join-Path $tempDir 'missing.stderr.log'
        $missingRun = Invoke-OwnedProcess (Join-Path $tempDir 'does-not-exist.exe') '-test.v' 5 $tempDir $missingOut $missingErr

        if ($exitRun.ExitCode -ne 7 -or $exitRun.TimedOut -or $exitRun.KillFailed -or
            $exitRun.JoinTimedOut -or $exitRun.Error.Length -gt 0 -or -not $exitMarkers) {
            Write-Host ('self-test: FAIL (exit/stderr capture; error=' + $exitRun.Error + ')')
            $selfExit = 1
        }
        elseif (-not $hangRun.TimedOut -or $hangRun.KillFailed -or $hangRun.JoinTimedOut -or
            $hangRun.Error.Length -gt 0 -or -not $hangMarkers -or $hangRun.Pid -le 0) {
            Write-Host ('self-test: FAIL (timeout/kill: timedOut=' + $hangRun.TimedOut +
                ' killFailed=' + $hangRun.KillFailed + ' joinTimedOut=' + $hangRun.JoinTimedOut +
                ' error=' + $hangRun.Error + ' pid=' + $hangRun.Pid + ')')
            $selfExit = 1
        }
        elseif ($earlyRun.ExitCode -ne 1 -or $earlyRun.TimedOut -or -not $earlyMarkers -or
            $earlyRun.Error.Length -gt 0) {
            Write-Host ('self-test: FAIL (early exit; code=' + $earlyRun.ExitCode +
                ' error=' + $earlyRun.Error + ')')
            $selfExit = 1
        }
        elseif ($missingRun.Error.Length -eq 0 -or $missingRun.Pid -ne -1 -or
            $missingRun.TimedOut -or $missingRun.KillFailed -or $missingRun.JoinTimedOut) {
            Write-Host ('self-test: FAIL (start exception recovery; error=' + $missingRun.Error +
                ' pid=' + $missingRun.Pid + ')')
            $selfExit = 1
        }
        else {
            Write-Host ('self-test: PASS (exit7+stderr; hang timeout/kill; early exit1; ' +
                'start exception; stdout/stderr preserved)')
        }
    }
    catch {
        Write-Host ('self-test: FAIL (' + $_.Exception.Message + ')')
        $selfExit = 1
    }
    finally {
        Write-Host ('self-test logs kept: ' + $tempDir)
    }
    exit $selfExit
}

try {
    $null = Read-Manifest $PackageDir
}
catch {
    Write-Host ('FAIL: ' + $_.Exception.Message)
    exit 2
}
Write-Host 'manifest/hash check: PASS'
if ($CheckOnly) {
    exit 0
}

$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
$resultDir = Join-Path $PackageDir ('results-winpipe-' + $stamp)
$suffix = 1
while (Test-Path -LiteralPath $resultDir) {
    $suffix = $suffix + 1
    $resultDir = Join-Path $PackageDir ('results-winpipe-' + $stamp + '-' + $suffix)
}
New-Item -ItemType Directory -Path $resultDir | Out-Null

$tests = @('winpipe.test.exe', 'winrelay.test.exe')
$summary = @()
$failed = $false
foreach ($exe in $tests) {
    $stdoutPath = Join-Path $resultDir ($exe + '.stdout.log')
    $stderrPath = Join-Path $resultDir ($exe + '.stderr.log')
    $runnerError = ''
    $run = [pscustomobject]@{
        ExitCode = -1; TimedOut = $false; KillFailed = $false; JoinTimedOut = $false
        Pid = -1; Error = ''
    }
    try {
        $run = Invoke-OwnedProcess (Join-Path $PackageDir $exe) '-test.v -test.timeout 120s' 180 $PackageDir $stdoutPath $stderrPath
    }
    catch {
        $runnerError = $_.Exception.Message
    }
    $stdoutText = ''
    $stderrText = ''
    if (Test-Path -LiteralPath $stdoutPath) {
        $stdoutText = Get-Content -LiteralPath $stdoutPath -Raw -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $stderrPath) {
        $stderrText = Get-Content -LiteralPath $stderrPath -Raw -ErrorAction SilentlyContinue
    }
    if ($null -eq $stdoutText) { $stdoutText = '' }
    if ($null -eq $stderrText) { $stderrText = '' }
    $pass = [regex]::Matches($stdoutText, '(?m)^--- PASS').Count
    $fail = [regex]::Matches($stdoutText, '(?m)^--- FAIL').Count
    $skip = [regex]::Matches($stdoutText, '(?m)^--- SKIP').Count
    $stderrNonEmpty = $stderrText.Trim().Length -gt 0 -or $runnerError.Length -gt 0
    $summary += ($exe + ' exit=' + $run.ExitCode + ' pass=' + $pass + ' fail=' + $fail +
        ' notrun=' + $skip + ' timeout=' + $run.TimedOut + ' killfail=' + $run.KillFailed +
        ' joinfail=' + $run.JoinTimedOut + ' stderr=' + $stderrNonEmpty + ' pid=' + $run.Pid +
        ' error=' + $run.Error + ' stdout=' + $stdoutPath + ' stderrfile=' + $stderrPath)
    if ($runnerError.Length -gt 0) {
        $summary += ('runner error for ' + $exe + ': ' + $runnerError)
    }
    if ($run.ExitCode -ne 0 -or $run.TimedOut -or $run.KillFailed -or $run.JoinTimedOut -or
        $run.Error.Length -gt 0 -or $stderrNonEmpty) {
        $failed = $true
    }
}
$summary | Out-File -LiteralPath (Join-Path $resultDir 'summary.txt') -Encoding utf8
$summary | ForEach-Object { Write-Host $_ }
Write-Host ('results: ' + $resultDir)
if ($failed) {
    exit 1
}
exit 0
