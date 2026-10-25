param([switch]$ResumePrepared)
# M2 Windows Terminal 검증 — M2 승인된 사용자 1회 검증. 일반 사용자 PowerShell 5.1, 관리자 아님.
# native unit → M2 준비 → 같은 탭에서 enter test 2회 → 수동 확인 → 자기 UUID poweroff → 결과 회수.
# 원 M1 owner/예약/VDI/이미지/로그는 수정·삭제하지 않는다. SSH/서비스/방화벽 변경 없음.
$ErrorActionPreference = 'Stop'
$targetUuid = '8173a191-47a9-497f-a254-5873a31cef0d'
$targetName = 'vboxdistro-m1-3487a95a'
$targetRun = '3487a95a-a207-463b-8a4b-22769e50906f'
$originalOwnerSha = '0113a97fa36bcd44e60f34b4753f20787c747d16ad7bd5b98b1cdcffa0d7d143'
$m2ImageSha = '7abb333342a92a8e6494ec52de5967e579de6b2eb6780dd129e9493f1ff23fde'
$m2ImageSize = 67108864
$local = $env:LOCALAPPDATA
$runDir = Join-Path (Join-Path $local 'vboxdistro\m1-test') $targetRun
$m2Dir = Join-Path $runDir 'm2-unicode'
$ownerPath = Join-Path $runDir 'owner.json'
$metaPath = Join-Path $m2Dir 'm2.json'
$expectedCfg = Join-Path (Join-Path $runDir $targetName) ($targetName + '.vbox')
$reservation = Join-Path $runDir 'm2-unicode-prep.reservation'
$resumeMarker = Join-Path $runDir 'm2-unicode-resume.reservation'
$localResultName = 'result.json'
if ($ResumePrepared) { $localResultName = 'result-mouse-fix-' + [Guid]::NewGuid().ToString() + '.json' }
$localResultPath = Join-Path $m2Dir $localResultName
$collectName = 'result-m2-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$collectDir = Join-Path $PSScriptRoot $collectName
$nativeLog = Join-Path $collectDir 'native-test.log'
$diagPath = Join-Path $collectDir 'prep-commands.log'
$script:VBox = $null
$prepError = $null
$cleanupError = $null
$artifactError = $null
$script:DiagError = $null
$attemptOwned = $false
$collectReady = $false
$nativeExit = $null
$cliExit1 = $null
$cliExit2 = $null
$stateAfterFirst = $null
$stateAfterSecond = $null
$cleanup = [ordered]@{ verified = $false; action = 'not-attempted'; exit_code = $null; final_state = $null; error = $null }
$manual = [ordered]@{ shell_basics = 'NOT RUN'; mouse_input = 'NOT RUN'; ctrl_c_alive = 'NOT RUN'
	exit_restore = 'NOT RUN'; reenter = 'NOT RUN'; second_session_rejected = 'NOT RUN' }

function Test-NoReparse {
	param([string]$Path)
	if (-not (Test-Path -LiteralPath $Path)) { return $true }
	$item = Get-Item -LiteralPath $Path -Force
	return (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0)
}
function Test-SafeFileName {
	param([string]$Name)
	if ([string]::IsNullOrWhiteSpace($Name)) { return $false }
	if ($Name -match '[\\/:*?"<>|]') { return $false }
	$stem = ($Name -split '\.')[0].TrimEnd(' ')
	return -not ($stem -match '^(?i:CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])$')
}
function Write-Diag {
	param([string]$Line)
	try { Add-Content -LiteralPath $diagPath -Encoding UTF8 -Value ((Get-Date).ToUniversalTime().ToString('HH:mm:ss.fff') + ' ' + $Line) }
	catch {
		if ($null -eq $script:DiagError) {
			$script:DiagError = $_.Exception.Message
			Write-Host ('WARNING: diagnostics write failed: ' + $script:DiagError)
		}
	}
}
function Invoke-VBox {
	param([string[]]$Arguments)
	Write-Diag ('VBoxManage ' + ($Arguments -join ' '))
	$previous = $ErrorActionPreference
	$ErrorActionPreference = 'Continue'
	try {
		$global:LASTEXITCODE = $null
		$out = & $script:VBox @Arguments 2>&1
		$code = $global:LASTEXITCODE
	} finally { $ErrorActionPreference = $previous }
	foreach ($entry in @($out)) { if ($null -ne $entry) { Write-Diag ('  ' + [string]$entry) } }
	if ($null -eq $code) { throw 'VBoxManage exit code unavailable' }
	if ($code -ne 0) { throw ('VBoxManage failed (exit {0}): {1}' -f $code, ($Arguments -join ' ')) }
	return @($out | ForEach-Object { [string]$_ })
}
function Get-Info {
	$map = @{}
	foreach ($line in (Invoke-VBox @('showvminfo', $targetUuid, '--machinereadable'))) {
		$i = $line.IndexOf('=')
		if ($i -le 0) { continue }
		$map[$line.Substring(0, $i).Trim('"')] = $line.Substring($i + 1).Trim('"').Replace('\\', '\').Replace('\"', '"')
	}
	return $map
}
function Assert-NoReparseChain {
	foreach ($candidate in @($local, $runDir, $m2Dir, $ownerPath, $expectedCfg, $metaPath)) {
		$probe = [IO.Path]::GetFullPath($candidate)
		while ($true) {
			if (-not (Test-NoReparse $probe)) { throw "reparse point in path chain: $probe" }
			$parent = [IO.Path]::GetDirectoryName($probe.TrimEnd('\'))
			if ([string]::IsNullOrEmpty($parent) -or $parent -eq $probe) { break }
			$probe = $parent
		}
	}
}
function Assert-Owner {
	$owner = Get-Content -LiteralPath $ownerPath -Raw | ConvertFrom-Json
	if ($owner.run_id -ne $targetRun -or $owner.vm_uuid -ne $targetUuid -or $owner.vm_name -ne $targetName) { throw 'owner identity mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($owner.base_folder), [IO.Path]::GetFullPath($runDir), 'OrdinalIgnoreCase')) { throw 'owner base_folder mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($owner.config_file), [IO.Path]::GetFullPath($expectedCfg), 'OrdinalIgnoreCase')) { throw 'owner config mismatch' }
	if ($owner.source_image_sha256 -ne $originalOwnerSha) { throw 'owner source hash mismatch' }
}
function Assert-VM {
	Assert-Owner
	Assert-NoReparseChain
	$info = Get-Info
	if ($info['UUID'] -ne $targetUuid -or $info['name'] -ne $targetName) { throw 'VM identity mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($info['CfgFile']), [IO.Path]::GetFullPath($expectedCfg), 'OrdinalIgnoreCase')) { throw 'VM config mismatch' }
	if ($info['cpus'] -ne '1' -or $info['memory'] -ne '512' -or $info['firmware'] -ne 'EFI64') { throw 'VM resources changed' }
	for ($i = 1; $i -le 8; $i++) { if ($info["nic$i"] -ne 'none') { throw "nic$i is not none" } }
	if ($info['uart1'] -ne '0x03f8,4' -or $info['uarttype1'] -ne '16550A') { throw 'UART hardware changed' }
	return $info
}
function Copy-Fixed {
	param([string]$Source, [string]$Name)
	if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) { return ($Name + ': missing') }
	$dest = Join-Path $collectDir $Name
	if (Test-Path -LiteralPath $dest) { return ($Name + ': destination exists') }
	try { Copy-Item -LiteralPath $Source -Destination $dest -ErrorAction Stop } catch { return ($Name + ': ' + $_.Exception.Message) }
	return $null
}
function Write-NewFile {
	param([string]$Path, [string]$Text)
	$bytes = [Text.Encoding]::UTF8.GetBytes($Text)
	$stream = [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
	try { $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Dispose() }
}
function Confirm {
	param([string]$Key, [string]$Text)
	$answer = Read-Host ($Text + ' 확인했으면 y')
	if ($answer -eq 'y') { $manual[$Key] = 'confirmed' } else { $manual[$Key] = 'not-confirmed' }
}
function Run-Native {
	param([string]$Exe, [string[]]$Arguments)
	$previous = $ErrorActionPreference
	$ErrorActionPreference = 'Continue'
	try {
		$global:LASTEXITCODE = $null
		$out = & $Exe @Arguments 2>&1
		$code = $global:LASTEXITCODE
	} finally { $ErrorActionPreference = $previous }
	if ($null -eq $code) { throw 'native exit code unavailable' }
	return @{ Output = @($out | ForEach-Object { [string]$_ }); Code = $code }
}

# 메모리 self-check: 예약/수집 이름과 고정 identity (OS 조회 없음)
foreach ($case in @(@('m2-unicode-prep.reservation', $true), @('m2-unicode-resume.reservation', $true), @('serial-console.log', $true), @('COM1.log', $false), @('NUL', $false), @('aux.txt', $false))) {
	if ((Test-SafeFileName $case[0]) -ne [bool]$case[1]) { throw ('filename self-check failed: ' + $case[0]) }
}
if ($targetUuid -notmatch '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$') { throw 'UUID constant malformed' }
if ($targetRun -notmatch '^[0-9a-f-]{36}$') { throw 'run id constant malformed' }
if ([IO.Path]::GetFileName($resumeMarker) -ne 'm2-unicode-resume.reservation') { throw 'resume marker filename changed' }
if ([IO.Path]::GetFileName($reservation) -ne 'm2-unicode-prep.reservation') { throw 'reservation filename changed' }
if (-not (Test-SafeFileName $localResultName) -or -not $localResultName.EndsWith('.json')) { throw 'local result filename is not safe json' }
if (-not (Test-SafeFileName $collectName)) { throw 'collect directory name is not a safe filename' }

try {
	if ([string]::IsNullOrEmpty($local) -or -not [IO.Path]::IsPathRooted($local)) { throw 'LOCALAPPDATA must be an absolute path' }
	New-Item -ItemType Directory -Path $collectDir | Out-Null
	$collectReady = $true
	$vbox = Join-Path $env:ProgramFiles 'Oracle\VirtualBox\VBoxManage.exe'
	if (-not (Test-Path -LiteralPath $vbox -PathType Leaf)) { throw "VBoxManage not found: $vbox" }
	$script:VBox = $vbox
	$version = (Invoke-VBox @('--version') | Select-Object -First 1)
	if ($version -notmatch '^7\.2\.') { throw "VirtualBox 7.2.x required: $version" }
	$os = Get-CimInstance Win32_OperatingSystem
	if ([int64]$os.FreePhysicalMemory -lt 786432) { throw 'less than 768 MiB free memory' }
	$testExe = Join-Path $PSScriptRoot 'boxdwel-enter.test.exe'
	$cli = Join-Path $PSScriptRoot 'boxdwel.exe'
	foreach ($exe in @($testExe, $cli)) { if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { throw "missing package file: $exe" } }
	Write-Host 'native unit tests (pipe accept/timeout/close, no VM)...'
	$nativeArgs = @('-test.v', '-test.timeout=30s')
	if ($ResumePrepared) { $nativeArgs = @('-test.run=^TestRawInputMode$', '-test.v', '-test.timeout=30s') }
	if ($ResumePrepared -and -not ($nativeArgs -contains '-test.run=^TestRawInputMode$')) { throw 'resume native filter missing' }
	$native = Run-Native -Exe $testExe -Arguments $nativeArgs
	$native.Output | Out-File -LiteralPath $nativeLog -Encoding UTF8
	$nativeExit = $native.Code
	if ($nativeExit -ne 0) { throw "native unit tests failed (exit $nativeExit): $nativeLog" }
	if (-not (Test-Path -LiteralPath $runDir -PathType Container)) { throw "run directory missing: $runDir" }
	Assert-NoReparseChain
	Assert-Owner
	$info = Assert-VM
	if ($info['VMState'] -ne 'poweroff') { throw "VM must be poweroff before preparation: $($info['VMState'])" }
	if ($ResumePrepared) {
		$preparedImage = Join-Path $m2Dir 'm1-boot.img'
		$preparedVdi = Join-Path $m2Dir 'm1-boot.vdi'
		foreach ($prepared in @($reservation, $metaPath, $preparedImage, $preparedVdi)) {
			if (-not (Test-Path -LiteralPath $prepared -PathType Leaf)) { throw "prepared file missing: $prepared" }
			if (-not (Test-NoReparse $prepared)) { throw "prepared file is a reparse point: $prepared" }
		}
		$prepMarker = Get-Content -LiteralPath $reservation -Raw | ConvertFrom-Json
		if ($prepMarker.run_id -ne $targetRun -or $prepMarker.image_sha256 -ne $m2ImageSha) { throw 'existing prep reservation is not for the approved M2 image' }
		$meta = Get-Content -LiteralPath $metaPath -Raw | ConvertFrom-Json
		if ($meta.schema -ne 'm2' -or $meta.run_id -ne $targetRun -or $meta.vm_uuid -ne $targetUuid -or $meta.vm_name -ne $targetName) { throw 'm2.json identity mismatch' }
		if ($meta.original_owner_sha256 -ne $originalOwnerSha -or $meta.image_sha256 -ne $m2ImageSha) { throw 'm2.json hash binding mismatch' }
		if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.image_path), [IO.Path]::GetFullPath($preparedImage), 'OrdinalIgnoreCase')) { throw 'm2.json image_path mismatch' }
		if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.vdi_path), [IO.Path]::GetFullPath($preparedVdi), 'OrdinalIgnoreCase')) { throw 'm2.json vdi_path mismatch' }
		if ((Get-Item -LiteralPath $preparedImage -Force).Length -ne $m2ImageSize) { throw 'prepared M2 image size mismatch' }
		if ((Get-FileHash -LiteralPath $preparedImage -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m2ImageSha) { throw 'prepared M2 image hash mismatch' }
		$info = Assert-VM
		if ($info['VMState'] -ne 'poweroff') { throw "resume requires poweroff; state=$($info['VMState'])" }
		if (-not [string]::Equals([IO.Path]::GetFullPath($info['SATA-0-0']), [IO.Path]::GetFullPath($preparedVdi), 'OrdinalIgnoreCase')) { throw 'SATA port 0 is not the prepared M2 VDI' }
		if (Test-Path -LiteralPath $resumeMarker) { throw "unicode resume reservation exists: $resumeMarker" }
		Write-NewFile -Path $resumeMarker -Text (ConvertTo-Json -Compress ([ordered]@{ run_id = $targetRun; image_sha256 = $m2ImageSha; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
		$attemptOwned = $true
	} else {
		$image = Join-Path $PSScriptRoot 'm1-boot.img'
		if ((Get-Item -LiteralPath $image -Force).Length -ne $m2ImageSize) { throw 'M2 image size mismatch' }
		if ((Get-FileHash -LiteralPath $image -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m2ImageSha) { throw 'M2 image SHA-256 mismatch' }
		if (Test-Path -LiteralPath $reservation) { throw "M2 preparation reservation exists: $reservation" }
		Write-NewFile -Path $reservation -Text (ConvertTo-Json -Compress ([ordered]@{ run_id = $targetRun; image_sha256 = $m2ImageSha; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
		New-Item -ItemType Directory -Path $m2Dir | Out-Null
		$attemptOwned = $true
		Assert-NoReparseChain
		$localImage = Join-Path $m2Dir 'm1-boot.img'
		[IO.File]::Copy($image, $localImage, $false)
		if ((Get-FileHash -LiteralPath $localImage -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m2ImageSha) { throw 'copied M2 image hash mismatch' }
		$vdi = Join-Path $m2Dir 'm1-boot.vdi'
		Assert-VM | Out-Null
		Invoke-VBox @('convertfromraw', $localImage, $vdi, '--format', 'VDI') | Out-Null
		Write-NewFile -Path $metaPath -Text (ConvertTo-Json -Depth 3 ([ordered]@{ schema = 'm2'; run_id = $targetRun
			vm_uuid = $targetUuid; vm_name = $targetName; original_owner_sha256 = $originalOwnerSha
			image_sha256 = $m2ImageSha; image_path = $localImage; vdi_path = $vdi }))
		Assert-VM | Out-Null
		Invoke-VBox @('storageattach', $targetUuid, '--storagectl', 'SATA', '--port', '0', '--device', '0', '--type', 'hdd', '--medium', $vdi) | Out-Null
		$info = Assert-VM
		if (-not [string]::Equals([IO.Path]::GetFullPath($info['SATA-0-0']), [IO.Path]::GetFullPath($vdi), 'OrdinalIgnoreCase')) { throw 'SATA port 0 does not reference the M2 VDI' }
	}

	Write-Host ''
	Write-Host 'M2 준비 완료. 아래 순서로 같은 탭에서 검증합니다.'
	Write-Host '첫 셸이 열려 있는 동안에만 다른 탭에서 동시 접속 거부를 확인하세요(그때만 두 번째 실행이 거부됩니다):'
	Write-Host ('  & "' + $cli + '" enter test   -> "another session may be active" 오류')
	Write-Host '같은 탭 첫 실행:'
	Write-Host ('  & "' + $cli + '" enter test')
	Write-Host '  셸에서 아래를 각각 실행하세요:'
	Write-Host '    echo OK'
	Write-Host '    uname -a'
	Write-Host '    printf "\033[31mRED\033[0m\n"'
	Write-Host '    echo "한글 UTF-8"'
	Write-Host '  [확인] Backspace로 글자를 지우고 다시 입력되는지 확인하세요.'
	Write-Host '  sleep 60  -> Ctrl+C  ->  echo ALIVE  ->  exit'
	Write-Host '첫 셸에서 마우스를 움직여 좌표 문자열이 들어오는지 확인한 뒤 exit 하세요. 한글 편집줄이 ??이면 Ctrl+C/exit 후 shell_basics를 n으로 답하세요. 문제가 있으면 두 번째 접속 없이 정리합니다.'
	Assert-Owner
	$previous = $ErrorActionPreference
	$ErrorActionPreference = 'Continue'
	try { $global:LASTEXITCODE = $null; & $cli enter test; $cliExit1 = $global:LASTEXITCODE }
	finally { $ErrorActionPreference = $previous }
	if ($null -eq $cliExit1) { throw 'first enter exit code unavailable' }
	$stateAfterFirst = (Assert-VM)['VMState']
	if ($cliExit1 -ne 0) { throw "first enter test failed (exit $cliExit1)" }
	if ($stateAfterFirst -ne 'running') { throw "VM not running after first exit: $stateAfterFirst" }
	Confirm 'mouse_input' '마우스를 움직여도 셸에 좌표 문자열이 들어오지 않음'
	if ($manual['mouse_input'] -ne 'confirmed') { throw 'mouse input check failed or not confirmed; stopping before the second session' }
	Confirm 'shell_basics' 'echo/uname/ANSI, 한글 편집줄+실행출력 정상, backspace 기본 동작'
	if ($manual['shell_basics'] -ne 'confirmed') { throw 'shell basics or Hangul line editing failed; stopping before the second session' }
	Write-Host ''
	Write-Host 'PowerShell 프롬프트에서 echo/입력과 커서가 정상인지 확인하세요(콘솔 복원 관찰).'
	Read-Host '복원을 확인했으면 Enter' | Out-Null
	Write-Host '같은 탭 재진입:'
	Write-Host ('  & "' + $cli + '" enter test')
	Write-Host '  셸에서: echo REENTER  ->  exit'
	$previous = $ErrorActionPreference
	$ErrorActionPreference = 'Continue'
	try { $global:LASTEXITCODE = $null; & $cli enter test; $cliExit2 = $global:LASTEXITCODE }
	finally { $ErrorActionPreference = $previous }
	if ($null -eq $cliExit2) { throw 'second enter exit code unavailable' }
	$stateAfterSecond = (Assert-VM)['VMState']
	if ($cliExit2 -ne 0) { throw "second enter test failed (exit $cliExit2)" }
	if ($stateAfterSecond -ne 'running') { throw "VM not running after second exit: $stateAfterSecond" }

	Write-Host ''
	Write-Host '수동 확인(자동 PASS 아님). 실제로 확인한 항목만 y:'
	Confirm 'ctrl_c_alive' 'sleep 60 중 Ctrl+C 후 echo ALIVE 동작'
	Confirm 'exit_restore' 'exit 후 PowerShell echo/입력 복원'
	Confirm 'reenter' '같은 VM 재진입 성공'
	Confirm 'second_session_rejected' '첫 셸이 열린 동안 두 번째 접속이 거부됨'
} catch {
	$prepError = $_.Exception.Message
	Write-Host ('ERROR: ' + $prepError)
} finally {
	if ($attemptOwned -and $null -ne $script:VBox) {
		try {
			$info = Assert-VM
			$cleanup.final_state = $info['VMState']
			if ($info['VMState'] -eq 'running' -or $info['VMState'] -eq 'paused') {
				$previous = $ErrorActionPreference
				$ErrorActionPreference = 'Continue'
				try {
					$global:LASTEXITCODE = $null
					$out = & $script:VBox 'controlvm' $targetUuid 'poweroff' 2>&1
					$cleanup.exit_code = $global:LASTEXITCODE
				} finally { $ErrorActionPreference = $previous }
				foreach ($entry in @($out)) { if ($null -ne $entry) { Write-Diag ('  ' + [string]$entry) } }
				$cleanup.action = 'poweroff'
				if ($null -eq $cleanup.exit_code -or $cleanup.exit_code -ne 0) { throw ('poweroff failed (exit {0})' -f $cleanup.exit_code) }
				for ($i = 0; $i -lt 15; $i++) {
					$cleanup.final_state = (Get-Info)['VMState']
					if ($cleanup.final_state -ne 'running' -and $cleanup.final_state -ne 'paused') { break }
					Start-Sleep -Seconds 1
				}
			} else { $cleanup.action = 'none' }
			$cleanup.verified = $true
			if ($cleanup.final_state -eq 'running' -or $cleanup.final_state -eq 'paused') { throw ('VM is still {0}' -f $cleanup.final_state) }
		} catch {
			$cleanupError = $_.Exception.Message
			$cleanup.error = $cleanupError
			Write-Diag ('CLEANUP ERROR: ' + $cleanupError)
		}
	}
	$artifacts = @()
	if ($collectReady) {
		foreach ($pair in @(@($ownerPath, 'owner.json'), @($metaPath, 'm2.json'),
				@((Join-Path (Join-Path $runDir $targetName) 'Logs\VBox.log'), 'VBox.log'))) {
			$err = Copy-Fixed -Source $pair[0] -Name $pair[1]
			if ($null -ne $err) { $artifacts += $err }
		}
		if (-not (Test-Path -LiteralPath $nativeLog)) { $artifacts += 'native-test.log: missing' }
		if (-not (Test-Path -LiteralPath $diagPath)) { $artifacts += 'prep-commands.log: missing' }
	}
	if ($artifacts.Count -gt 0) { $artifactError = ($artifacts -join '; ') }
	$pass = ($null -eq $prepError -and $null -eq $cleanupError -and $null -eq $script:DiagError -and
		$null -eq $artifactError -and $nativeExit -eq 0 -and $cliExit1 -eq 0 -and $cliExit2 -eq 0 -and
		$stateAfterFirst -eq 'running' -and $stateAfterSecond -eq 'running' -and
		$cleanup.verified -and $cleanup.final_state -eq 'poweroff' -and
		-not ($manual.Values -contains 'not-confirmed') -and -not ($manual.Values -contains 'NOT RUN'))
	$result = [ordered]@{ run_id = $targetRun; vm_uuid = $targetUuid; m2_dir = $m2Dir; image_sha256 = $m2ImageSha
		mode = $(if ($ResumePrepared) { 'resume-prepared' } else { 'fresh-prepare' })
		native_test_exit = $nativeExit; first_exit = $cliExit1; second_exit = $cliExit2
		state_after_first = $stateAfterFirst; state_after_second = $stateAfterSecond
		prep_error = $prepError; cleanup = $cleanup; diagnostics_error = $script:DiagError
		manual = $manual; artifact_error = $artifactError; pass = $pass }
	$resultText = ConvertTo-Json -Depth 6 $result
	$localResult = $null
	$sharedResult = $null
	$writeErrors = @()
	if ($attemptOwned) {
		$localResult = $localResultPath
		try { Write-NewFile -Path $localResult -Text $resultText }
		catch { $writeErrors += ('local result: ' + $_.Exception.Message); $localResult = $null }
	}
	if ($collectReady) {
		$sharedResult = Join-Path $collectDir 'result.json'
		try { Write-NewFile -Path $sharedResult -Text $resultText }
		catch { $writeErrors += ('shared result: ' + $_.Exception.Message); $sharedResult = $null }
	}
	if ($writeErrors.Count -gt 0) {
		foreach ($line in $writeErrors) { [Console]::Error.WriteLine($line) }
		$cause = ($writeErrors -join '; ')
		if ($null -eq $artifactError) { $artifactError = $cause } else { $artifactError = $artifactError + '; ' + $cause }
		$pass = $false
		$result['pass'] = $false
		$result['artifact_error'] = $artifactError
		$resultText = ConvertTo-Json -Depth 6 $result
		foreach ($path in @($localResult, $sharedResult)) {
			if ($null -ne $path) {
				try { [IO.File]::WriteAllText($path, $resultText, (New-Object Text.UTF8Encoding($false))) }
				catch { [Console]::Error.WriteLine('result update failed: ' + $path + ': ' + $_.Exception.Message) }
			}
		}
		Write-Host 'WARNING: 결과 파일 저장에 실패했습니다. 완료로 표시하지 않습니다.' -ForegroundColor Yellow
	} else {
		if ($null -ne $sharedResult) { Write-Host ('M2 result: ' + $sharedResult) }
	}
	Write-Host $resultText
}
if (-not $pass) { exit 1 }
exit 0
