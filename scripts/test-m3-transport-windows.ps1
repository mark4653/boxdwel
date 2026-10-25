# M3 COM2 전송 사전 실험 — Windows 사용자 측정 (PowerShell 5.1, 관리자 아님).
# 이번 수정은 startup 순서: COM2 disconnected로 부팅 → COM1 M3_PHASE1_START 확인 → host pipe 시작.
# -ResumePrepared: 기존 m3-transport-r1 준비물을 재사용한다(새 bundle 필수 경로).
# -SelfTest: VBox/VM/tempfile 없이 연결 순서와 Source/Name 목록만 메모리에서 검사한다.
# 중간 frame 강제 단절/자동 복구/parser resync는 측정하지 않는다(NOT RUN). 성능 threshold 없음.
param([switch]$ResumePrepared, [switch]$SelfTest)
$ErrorActionPreference = 'Stop'
$targetUuid = '8173a191-47a9-497f-a254-5873a31cef0d'
$targetName = 'vboxdistro-m1-3487a95a'
$targetRun = '3487a95a-a207-463b-8a4b-22769e50906f'
$originalOwnerSha = '0113a97fa36bcd44e60f34b4753f20787c747d16ad7bd5b98b1cdcffa0d7d143'
$m2ImageSha = '7abb333342a92a8e6494ec52de5967e579de6b2eb6780dd129e9493f1ff23fde'
$m2ImageSize = 67108864
$m3ImageSha = '46bcc05953b26440f664c9385305c368f6584d792479e4031694cbc413dd8963'
$m3ImageSize = 67108864
$pipeName = '\\.\pipe\vboxdistro-m3-probe'

$local = $env:LOCALAPPDATA
$runDir = Join-Path (Join-Path $local 'vboxdistro\m1-test') $targetRun
$m3Dir = Join-Path $runDir 'm3-transport-r1'
$ownerPath = Join-Path $runDir 'owner.json'
$m2Dir = Join-Path $runDir 'm2-unicode'
$metaPath = Join-Path $m2Dir 'm2.json'
$expectedCfg = Join-Path (Join-Path $runDir $targetName) ($targetName + '.vbox')
$vmDir = Join-Path $runDir $targetName
$vboxLog = Join-Path (Join-Path $vmDir 'Logs') 'VBox.log'
$prepReservation = Join-Path $runDir 'm3-transport-r1.reservation'
$resumeReservation = Join-Path $runDir 'm3-transport-r1-startup-fix.reservation'
$m3Image = Join-Path $m3Dir 'm1-boot.img'
$m3Vdi = Join-Path $m3Dir 'm1-boot.vdi'
$m3Meta = Join-Path $m3Dir 'm3.json'
$originalSerial = Join-Path $m3Dir 'serial-console.log'
$fixedSerial = Join-Path $m3Dir 'serial-console-startup-fix.log'
$startupRecord = Join-Path $m3Dir 'startup-fix.json'
$serialPath = $(if ($ResumePrepared) { $fixedSerial } else { $originalSerial })
$m3ImageSrc = Join-Path $PSScriptRoot 'm1-boot.img'
$hostExe = Join-Path $PSScriptRoot 'm3-transport-probe.exe'
$nativeExe = Join-Path $PSScriptRoot 'boxdwel-enter.test.exe'
$collectName = 'result-m3-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$collectDir = Join-Path $PSScriptRoot $collectName
$nativeLog = Join-Path $collectDir 'native-test.log'
$diagPath = Join-Path $collectDir 'commands.log'
$script:VBox = $null
$script:DiagError = $null
$script:CleanupProcError = $null
$script:M2Vdi = $null
$prepError = $null
$cleanupError = $null
$artifactError = $null
$serialError = $null
$collectReady = $false
$reservationOwned = $false
$attached = $false
$nativeExit = $null
$phase1Exit = $null
$phase2Exit = $null
$phase1Complete = $false
$phase2Complete = $false
$stateAfterPhase1 = $null
$stateAfterPhase2 = $null
$p1Report = $null
$p2Report = $null
$phase1Proc = $null
$phase2Proc = $null
$m2Vdi = $null
$cleanup = [ordered]@{ verified = $false; action = 'not-attempted'; exit_code = $null; final_state = $null; error = $null }

function Test-NoReparse {
	param([string]$Path)
	if (-not (Test-Path -LiteralPath $Path)) { return $true }
	$item = Get-Item -LiteralPath $Path -Force
	return (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0)
}
function Assert-NoReparseChain {
	param([string[]]$Paths)
	foreach ($candidate in $Paths) {
		if ([string]::IsNullOrEmpty($candidate)) { continue }
		$probe = [IO.Path]::GetFullPath($candidate)
		while ($true) {
			if (-not (Test-NoReparse $probe)) { throw "reparse point in path chain: $probe" }
			$parent = [IO.Path]::GetDirectoryName($probe.TrimEnd('\'))
			if ([string]::IsNullOrEmpty($parent) -or $parent -eq $probe) { break }
			$probe = $parent
		}
	}
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
function Assert-Owner {
	$owner = Get-Content -LiteralPath $ownerPath -Raw | ConvertFrom-Json
	if ($owner.run_id -ne $targetRun -or $owner.vm_uuid -ne $targetUuid -or $owner.vm_name -ne $targetName) { throw 'owner identity mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($owner.base_folder), [IO.Path]::GetFullPath($runDir), 'OrdinalIgnoreCase')) { throw 'owner base_folder mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($owner.config_file), [IO.Path]::GetFullPath($expectedCfg), 'OrdinalIgnoreCase')) { throw 'owner config mismatch' }
	if ($owner.source_image_sha256 -ne $originalOwnerSha) { throw 'owner source hash mismatch' }
}
function Assert-M2Meta {
	$meta = Get-Content -LiteralPath $metaPath -Raw | ConvertFrom-Json
	if ($meta.schema -ne 'm2' -or $meta.run_id -ne $targetRun -or $meta.vm_uuid -ne $targetUuid -or $meta.vm_name -ne $targetName) { throw 'm2.json identity mismatch' }
	if ($meta.original_owner_sha256 -ne $originalOwnerSha -or $meta.image_sha256 -ne $m2ImageSha) { throw 'm2.json hash binding mismatch' }
	$image = Join-Path $m2Dir 'm1-boot.img'
	$vdi = Join-Path $m2Dir 'm1-boot.vdi'
	if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.image_path), [IO.Path]::GetFullPath($image), 'OrdinalIgnoreCase')) { throw 'm2.json image_path mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.vdi_path), [IO.Path]::GetFullPath($vdi), 'OrdinalIgnoreCase')) { throw 'm2.json vdi_path mismatch' }
	if (-not (Test-Path -LiteralPath $image -PathType Leaf) -or -not (Test-Path -LiteralPath $vdi -PathType Leaf)) { throw 'M2 image/VDI missing' }
	if (-not (Test-NoReparse $image) -or -not (Test-NoReparse $vdi)) { throw 'M2 image/VDI is a reparse point' }
	Assert-NoReparseChain @($m2Dir, $image, $vdi)
	if ((Get-Item -LiteralPath $image -Force).Length -ne $m2ImageSize) { throw 'M2 image size mismatch' }
	if ((Get-FileHash -LiteralPath $image -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m2ImageSha) { throw 'M2 image hash mismatch' }
	$script:M2Vdi = $vdi
	return $meta
}
function Assert-M3MetaResume {
	if (-not (Test-Path -LiteralPath $m3Meta -PathType Leaf)) { throw 'm3.json missing for resume' }
	if (-not (Test-Path -LiteralPath $prepReservation -PathType Leaf)) { throw 'm3-transport-r1.reservation missing for resume' }
	$meta = Get-Content -LiteralPath $m3Meta -Raw | ConvertFrom-Json
	if ($meta.schema -ne 'm3-transport' -or $meta.run_id -ne $targetRun -or $meta.vm_uuid -ne $targetUuid -or $meta.vm_name -ne $targetName) { throw 'm3.json identity mismatch' }
	if ($meta.original_owner_sha256 -ne $originalOwnerSha -or $meta.m3_image_sha256 -ne $m3ImageSha) { throw 'm3.json hash binding mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.m3_raw_path), [IO.Path]::GetFullPath($m3Image), 'OrdinalIgnoreCase')) { throw 'm3.json raw path mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath([string]$meta.m3_vdi_path), [IO.Path]::GetFullPath($m3Vdi), 'OrdinalIgnoreCase')) { throw 'm3.json vdi path mismatch' }
	$prep = Get-Content -LiteralPath $prepReservation -Raw | ConvertFrom-Json
	if ($prep.run_id -ne $targetRun -or $prep.image_sha256 -ne $m3ImageSha) { throw 'prep reservation mismatch' }
	if (-not (Test-Path -LiteralPath $m3Image -PathType Leaf) -or -not (Test-Path -LiteralPath $m3Vdi -PathType Leaf)) { throw 'prepared M3 image/VDI missing' }
	if ((Get-Item -LiteralPath $m3Image -Force).Length -ne $m3ImageSize) { throw 'prepared M3 raw size mismatch' }
	if ((Get-FileHash -LiteralPath $m3Image -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m3ImageSha) { throw 'prepared M3 raw hash mismatch' }
	Assert-NoReparseChain @($m3Dir, $m3Meta, $m3Image, $m3Vdi, $prepReservation)
	return $meta
}
function Assert-VM {
	Assert-Owner
	Assert-NoReparseChain @($local, $runDir, $m3Dir, $ownerPath, $expectedCfg, $metaPath, $m2Dir, $collectDir, $hostExe, $nativeExe)
	$info = Get-Info
	if ($info['UUID'] -ne $targetUuid -or $info['name'] -ne $targetName) { throw 'VM identity mismatch' }
	if (-not [string]::Equals([IO.Path]::GetFullPath($info['CfgFile']), [IO.Path]::GetFullPath($expectedCfg), 'OrdinalIgnoreCase')) { throw 'VM config mismatch' }
	if ($info['cpus'] -ne '1' -or $info['memory'] -ne '512' -or $info['firmware'] -ne 'EFI64') { throw 'VM resources changed' }
	for ($i = 1; $i -le 8; $i++) { if ($info["nic$i"] -ne 'none') { throw "nic$i is not none" } }
	if ($info['uart1'] -ne '0x03f8,4' -or $info['uarttype1'] -ne '16550A') { throw 'UART1 hardware changed' }
	return $info
}
function Assert-VMState {
	param([string]$ExpectedSata = '')
	$info = Assert-VM
	$current = [string]$info['SATA-0-0']
	if (-not [string]::IsNullOrEmpty($current)) { Assert-NoReparseChain @($current) }
	if (-not [string]::IsNullOrEmpty($ExpectedSata)) {
		if (-not [string]::Equals([IO.Path]::GetFullPath($current), [IO.Path]::GetFullPath($ExpectedSata), 'OrdinalIgnoreCase')) {
			throw ('SATA port 0 is not the expected VDI: ' + $current)
		}
		Assert-NoReparseChain @($ExpectedSata)
	}
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
function Read-SharedText {
	param([string]$Path)
	$share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
	$stream = New-Object IO.FileStream($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, $share)
	try {
		$reader = New-Object IO.StreamReader($stream, [Text.Encoding]::UTF8)
		try { return $reader.ReadToEnd() } finally { $reader.Dispose() }
	} finally { $stream.Dispose() }
}
function Get-SerialText {
	param([string]$Path)
	if (-not (Test-Path -LiteralPath $Path)) { return '' }
	try { return (Read-SharedText $Path) } catch [IO.IOException] { return '' }
}
function Test-MarkerLine {
	param([string]$Text, [string]$Marker)
	return ($Text -match ('(?m)^' + [regex]::Escape($Marker) + '\r?$'))
}
function Test-SerialMarkers {
	param([string]$Path)
	$need = @('M3_PHASE1_EXIT rc=0', 'M3_PHASE2_EXIT rc=0', 'M3_PROBE_DONE')
	$deadline = (Get-Date).AddSeconds(3)
	$text = ''
	while ((Get-Date) -lt $deadline) {
		$text = Get-SerialText $Path
		$missing = @($need | Where-Object { $text -notmatch [regex]::Escape($_) })
		if ($missing.Count -eq 0) { return @() }
		Start-Sleep -Milliseconds 100
	}
	return @($need | Where-Object { $text -notmatch [regex]::Escape($_) })
}
function Wait-PipeReady {
	param([string]$Path, [int]$TimeoutMs)
	$deadline = (Get-Date).AddMilliseconds($TimeoutMs)
	while ((Get-Date) -lt $deadline) {
		if (Test-Path -LiteralPath $Path) {
			try {
				if ((Read-SharedText $Path) -match '(?m)^M3_PIPE_READY\r?$') { return $true }
			} catch [UnauthorizedAccessException] {
				throw ('cannot read probe stdout: ' + $_.Exception.Message)
			} catch [IO.IOException] { }
		}
		Start-Sleep -Milliseconds 200
	}
	return $false
}
function Start-HostProbe {
	param([string]$Report, [string]$Stdout, [string]$Stderr)
	if (Test-Path -LiteralPath $Report) { throw "report already exists: $Report" }
	$argLine = '-report "' + $Report + '"'
	$proc = Start-Process -FilePath $hostExe -ArgumentList $argLine -RedirectStandardOutput $Stdout -RedirectStandardError $Stderr -WindowStyle Hidden -PassThru
	if ($null -eq $proc) { throw 'Start-Process returned no process' }
	$null = $proc.Handle
	return $proc
}
function Stop-OwnProcess {
	param($Process)
	if ($null -eq $Process) { return }
	try {
		if (-not $Process.HasExited) {
			$Process.Kill()
			if (-not $Process.WaitForExit(5000)) { throw 'probe did not exit after kill' }
		}
	} catch {
		if ($null -eq $script:CleanupProcError) { $script:CleanupProcError = $_.Exception.Message }
	}
}
function Assert-PhaseReport {
	param([string]$Path, [string]$Phase)
	if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "$Phase report missing: $Path" }
	$j = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
	if ($j.protocol -ne 'm3-transport-spike-v1') { throw "$Phase protocol mismatch" }
	if ($j.measurement_complete -ne $true) { throw ("$Phase incomplete: " + [string]$j.error) }
	$r = $j.report
	if ($null -eq $r) { throw "$Phase report payload missing" }
	if ([string]::IsNullOrEmpty([string]$r.nonce_sha256)) { throw "$Phase nonce missing" }
	if ([int]$r.echo_samples -lt 1) { throw "$Phase echo samples missing" }
	if ([int]$r.stream_bytes_each -le 0 -or @($r.streams).Count -ne 2) { throw "$Phase stream summary missing" }
	foreach ($s in @($r.streams)) {
		if ([int]$s.bytes -ne [int]$r.stream_bytes_each) { throw "$Phase stream $($s.id) bytes mismatch" }
		if ([string]::IsNullOrEmpty([string]$s.sent_sha256) -or $s.sent_sha256 -ne $s.received_sha256) { throw "$Phase stream $($s.id) hash mismatch" }
	}
	if ([int]$r.interleave_switches -lt 2) { throw "$Phase interleave switches < 2" }
	if ([int]$r.pings_sent -ne ([int]$r.pings_ok + [int]$r.pings_missed)) { throw "$Phase ping count mismatch" }
	if (([int]$r.bulk_ping_samples + [int]$r.pings_after_stream) -ne [int]$r.pings_ok) { throw "$Phase bulk sample count mismatch" }
	if ([int64]$r.stream_ms -lt 0 -or [int64]$r.aggregate_bytes_per_sec -le 0) { throw "$Phase stream timing missing" }
	return $j
}
function Assert-Com2Disconnected {
	param($Info)
	if (-not $Info.ContainsKey('uartmode2') -or $Info['uartmode2'] -ne 'disconnected') {
		throw ('COM2 must be disconnected before start: ' + [string]$Info['uartmode2'])
	}
}
function New-CollectionPairs {
	param([string]$Owner, [string]$M2, [string]$M3, [string]$Serial, [string]$Startup, [string]$VBox)
	$list = New-Object System.Collections.Generic.List[object]
	$list.Add([pscustomobject]@{ Source = $Owner; Name = 'owner.json' })
	$list.Add([pscustomobject]@{ Source = $M2; Name = 'm2.json' })
	$list.Add([pscustomobject]@{ Source = $M3; Name = 'm3.json' })
	$list.Add([pscustomobject]@{ Source = $Serial; Name = (Split-Path -Leaf $Serial) })
	if (-not [string]::IsNullOrEmpty($Startup)) { $list.Add([pscustomobject]@{ Source = $Startup; Name = 'startup-fix.json' }) }
	$list.Add([pscustomobject]@{ Source = $VBox; Name = 'VBox.log' })
	return $list
}
# marker 확인 → host create → READY → VM 확인 → COM2 client 연결 → measure 종료 → report 검증.
# 의존 함수는 scriptblock으로 주입해 -SelfTest에서 VBox/VM 없이 순서를 검증한다.
function Invoke-PhaseFlow {
	param(
		[int]$Phase, [string]$SerialFile, [string]$HostReport, [string]$HostOut, [string]$HostErr,
		[scriptblock]$ReadSerial, [scriptblock]$StartHost, [scriptblock]$WaitReady,
		[scriptblock]$CheckVM, [scriptblock]$ConnectUart, [scriptblock]$WaitExit,
		[scriptblock]$ValidateReport, [int]$MarkerTimeoutSec = 90
	)
	$proc = $null
	try {
		$marker = 'M3_PHASE' + $Phase + '_START'
		$deadline = (Get-Date).AddSeconds($MarkerTimeoutSec)
		$seen = $false
		# timeout 0이어도 최소 1회는 실제로 읽는다.
		do {
			$text = [string](& $ReadSerial $SerialFile)
			if ($text -match 'M3_INIT_ERROR|M3_PHASE\d_FAIL') { throw ('guest init failure before phase ' + $Phase) }
			if (Test-MarkerLine $text $marker) { $seen = $true; break }
			if ((Get-Date) -ge $deadline) { break }
			Start-Sleep -Milliseconds 200
		} while ($true)
		if (-not $seen) { throw ('guest phase marker timeout: ' + $marker) }
		$events = New-Object System.Collections.Generic.List[string]
		$events.Add('marker')
		$proc = & $StartHost $HostReport $HostOut $HostErr
		$events.Add('host')
		if (-not (& $WaitReady $HostOut)) { throw ('host pipe ready timeout: phase ' + $Phase) }
		$events.Add('ready')
		& $CheckVM | Out-Null
		$events.Add('vmcheck')
		& $ConnectUart | Out-Null
		$events.Add('connect')
		if (-not (& $WaitExit $proc)) { throw ('measure timeout: phase ' + $Phase) }
		$events.Add('exit')
		$j = & $ValidateReport $HostReport ('phase' + $Phase)
		return [pscustomobject]@{ Process = $proc; Report = $j; Events = @($events) }
	} catch {
		if ($null -ne $proc) { Stop-OwnProcess $proc }
		throw
	}
}

if ($SelfTest) {
	# 1) Source/Name 고정 목록이 문자/4조각으로 flatten되지 않는지.
	$pairs = New-CollectionPairs -Owner 'C:\x\owner.json' -M2 'C:\x\m2.json' -M3 'C:\x\m3.json' -Serial 'C:\x\serial-console-startup-fix.log' -Startup 'C:\x\startup-fix.json' -VBox 'C:\x\VBox.log'
	$expected = 'owner.json,m2.json,m3.json,serial-console-startup-fix.log,startup-fix.json,VBox.log'
	if (($pairs.Count -ne 6) -or ((@($pairs | ForEach-Object { [string]$_.Name }) -join ',') -ne $expected)) { throw 'collection Source/Name list mismatch' }
	foreach ($p in $pairs) {
		if ($p -is [string] -or [string]::IsNullOrEmpty([string]$p.Source) -or ([string]$p.Name).Length -le 1) { throw 'collection list is flattened' }
	}
	# 2) 회수된 58-byte firmware만으로는 guest ready가 아니다(실제 read 1회, host create 0).
	$firmware = [Text.Encoding]::ASCII.GetString([byte[]](27, 91, 50, 74, 27, 91, 48, 49, 59, 48, 49, 72, 27, 91, 61, 51, 104, 27, 91, 50, 74, 27, 91, 48, 49, 59, 48, 49, 72, 27, 91, 50, 74, 27, 91, 48, 49, 59, 48, 49, 72, 27, 91, 61, 51, 104, 27, 91, 50, 74, 27, 91, 48, 49, 59, 48, 49, 72))
	$script:selfHost = 0
	$script:selfReads = 0
	$script:selfKills = 0
	$deadProc = [pscustomobject]@{ HasExited = $true; ExitCode = 0 }
	$startHost = { param($r, $o, $e) $script:selfHost++; return $deadProc }
	$ok = { param($a) $true }
	$report = { param($a, $b) [pscustomobject]@{ ok = $true } }
	try {
		Invoke-PhaseFlow -Phase 1 -SerialFile 'mem' -HostReport 'r' -HostOut 'o' -HostErr 'e' -ReadSerial { param($p) $script:selfReads++; $firmware } -StartHost $startHost -WaitReady $ok -CheckVM { } -ConnectUart { } -WaitExit $ok -ValidateReport $report -MarkerTimeoutSec 0 | Out-Null
		throw 'expected marker timeout for firmware-only input'
	} catch {
		if ($_.Exception.Message -notmatch 'marker timeout') { throw }
	}
	if ($script:selfReads -lt 1) { throw 'firmware fixture was never read' }
	if ($script:selfHost -ne 0) { throw 'host started before guest marker' }
	# 3) phase1 marker 이후에만 host create/connect, phase2는 phase1 marker로 통과하지 않는다.
	$phase1Text = "READY_BOOT_M1`nM3_PHASE1_START`n"
	$script:selfEvents = New-Object System.Collections.Generic.List[string]
	$flow = Invoke-PhaseFlow -Phase 1 -SerialFile 'mem' -HostReport 'r' -HostOut 'o' -HostErr 'e' -ReadSerial { param($p) $script:selfReads++; $phase1Text } -StartHost { param($r, $o, $e) $script:selfEvents.Add('host'); return $deadProc } -WaitReady { param($o) $script:selfEvents.Add('ready'); $true } -CheckVM { $script:selfEvents.Add('vmcheck') } -ConnectUart { $script:selfEvents.Add('connect') } -WaitExit { param($p) $script:selfEvents.Add('exit'); $true } -ValidateReport $report -MarkerTimeoutSec 5
	if (($flow.Events -join ',') -ne 'marker,host,ready,vmcheck,connect,exit') { throw ('phase1 sequence: ' + ($flow.Events -join ',')) }
	$script:selfHost = 0
	$script:selfReads = 0
	try {
		Invoke-PhaseFlow -Phase 2 -SerialFile 'mem' -HostReport 'r' -HostOut 'o' -HostErr 'e' -ReadSerial { param($p) $script:selfReads++; $phase1Text } -StartHost $startHost -WaitReady $ok -CheckVM { } -ConnectUart { } -WaitExit $ok -ValidateReport $report -MarkerTimeoutSec 0 | Out-Null
		throw 'expected phase2 marker timeout with phase1 text'
	} catch {
		if ($_.Exception.Message -notmatch 'marker timeout') { throw }
	}
	if ($script:selfReads -lt 1) { throw 'phase2 marker fixture was never read' }
	if ($script:selfHost -ne 0) { throw 'phase2 host started on phase1 marker' }
	# 4) start 이후 실패(measure timeout)면 helper가 own process를 kill한다.
	$script:selfKills = 0
	$liveProc = [pscustomobject]@{ HasExited = $false; ExitCode = 0 }
	$liveProc | Add-Member -MemberType ScriptMethod -Name Kill -Value { $script:selfKills++ }
	$liveProc | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value { param($ms) $true }
	try {
		Invoke-PhaseFlow -Phase 1 -SerialFile 'mem' -HostReport 'r' -HostOut 'o' -HostErr 'e' -ReadSerial { param($p) $phase1Text } -StartHost { param($r, $o, $e) return $liveProc } -WaitReady $ok -CheckVM { } -ConnectUart { } -WaitExit { param($p) $false } -ValidateReport $report -MarkerTimeoutSec 5 | Out-Null
		throw 'expected measure timeout'
	} catch {
		if ($_.Exception.Message -notmatch 'measure timeout') { throw }
	}
	if ($script:selfKills -ne 1) { throw 'own process was not killed on helper failure' }
	# 5) startvm 직전 COM2 disconnected gate.
	Assert-Com2Disconnected @{ uartmode2 = 'disconnected' }
	try {
		Assert-Com2Disconnected @{ uartmode2 = 'client' }
		throw 'expected COM2 disconnected gate failure'
	} catch {
		if ($_.Exception.Message -notmatch 'disconnected') { throw }
	}
	Write-Host 'SELFTEST PASS pairs=6 sequence=marker,host,ready,vmcheck,connect,exit'
	exit 0
}

try {
	if ([string]::IsNullOrEmpty($local) -or -not [IO.Path]::IsPathRooted($local)) { throw 'LOCALAPPDATA must be an absolute path' }
	if ($m3ImageSha -notmatch '^[0-9a-f]{64}$') { throw 'M3 image SHA constant is not bound' }
	New-Item -ItemType Directory -Path $collectDir | Out-Null
	$collectReady = $true
	Write-Diag ('collect=' + $collectDir)

	$vbox = Join-Path $env:ProgramFiles 'Oracle\VirtualBox\VBoxManage.exe'
	if (-not (Test-Path -LiteralPath $vbox -PathType Leaf)) { throw "VBoxManage not found: $vbox" }
	$script:VBox = $vbox
	$version = (Invoke-VBox @('--version') | Select-Object -First 1)
	if ($version -notmatch '^7\.2\.') { throw "VirtualBox 7.2.x required: $version" }
	$os = Get-CimInstance Win32_OperatingSystem
	if ([int64]$os.FreePhysicalMemory -lt 786432) { throw 'less than 768 MiB free memory' }

	Assert-Owner
	$chain = @($local, $runDir, $ownerPath, $expectedCfg, $metaPath, $vboxLog, $collectDir, $hostExe, $nativeExe, $m3Dir, $m3Meta, $m3Image, $m3Vdi, $prepReservation, $resumeReservation, $fixedSerial, $startupRecord)
	Assert-NoReparseChain $chain
	$preserve = New-Object System.Collections.Generic.List[object]
	$preserve.Add([pscustomobject]@{ Source = $ownerPath; Name = 'preserved-owner.json' })
	$preserve.Add([pscustomobject]@{ Source = $metaPath; Name = 'preserved-m2.json' })
	$preserve.Add([pscustomobject]@{ Source = $vboxLog; Name = 'preserved-VBox.log' })
	$preserve.Add([pscustomobject]@{ Source = $expectedCfg; Name = 'preserved-VM.vbox' })
	if ($ResumePrepared) {
		$preserve.Add([pscustomobject]@{ Source = $m3Meta; Name = 'preserved-m3.json' })
		$preserve.Add([pscustomobject]@{ Source = $originalSerial; Name = 'preserved-serial-console.log' })
	}
	$issues = @()
	foreach ($pair in $preserve) {
		$err = Copy-Fixed -Source $pair.Source -Name $pair.Name
		if ($null -ne $err) { $issues += $err }
	}
	if ($issues.Count -gt 0) { throw ('preflight preserve failed: ' + ($issues -join '; ')) }
	(Invoke-VBox @('showvminfo', $targetUuid, '--machinereadable')) | Out-File -LiteralPath (Join-Path $collectDir 'preserved-showvminfo.txt') -Encoding UTF8

	$required = @($hostExe, $nativeExe)
	if (-not $ResumePrepared) { $required += $m3ImageSrc }
	foreach ($file in $required) { if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "missing package file: $file" } }
	Assert-NoReparseChain @($hostExe, $nativeExe, $collectDir)
	$meta = Assert-M2Meta
	$m2Vdi = $script:M2Vdi

	Write-Host 'native pipe unit tests (no VM)...'
	$native = Run-Native -Exe $nativeExe -Arguments @('-test.v', '-test.timeout=60s')
	$native.Output | Out-File -LiteralPath $nativeLog -Encoding UTF8
	$nativeExit = $native.Code
	if ($nativeExit -ne 0) { throw "native unit tests failed (exit $nativeExit): $nativeLog" }

	if ($ResumePrepared) {
		$resumeMeta = Assert-M3MetaResume
		$info = Assert-VMState $m3Vdi
		if ($info['VMState'] -ne 'poweroff') { throw "resume requires poweroff: $($info['VMState'])" }
		if ($info['uart2'] -ne '0x02f8,3' -or $info['uarttype2'] -ne '16550A') { throw 'COM2 hardware mismatch for resume' }
		if (Test-Path -LiteralPath $resumeReservation) { throw "startup-fix reservation exists: $resumeReservation" }
		if (Test-Path -LiteralPath $fixedSerial) { throw "startup-fix COM1 exists: $fixedSerial" }
		if (Test-Path -LiteralPath $startupRecord) { throw "startup-fix record exists: $startupRecord" }
		Write-NewFile -Path $resumeReservation -Text (ConvertTo-Json -Compress ([ordered]@{
			run_id = $targetRun; image_sha256 = $m3ImageSha; mode = 'startup-fix'; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
		$reservationOwned = $true
		$attached = $true
		Write-NewFile -Path $startupRecord -Text (ConvertTo-Json -Depth 3 ([ordered]@{
			schema = 'm3-startup-fix'; run_id = $targetRun; vm_uuid = $targetUuid; vm_name = $targetName
			original_owner_sha256 = $originalOwnerSha; m3_image_sha256 = $m3ImageSha
			original_image = $m3Image; original_meta = $m3Meta; original_vdi = $m3Vdi; original_serial = $originalSerial
			com1_path = $fixedSerial; com2_pipe = $pipeName; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
		Assert-VMState $m3Vdi | Out-Null
		Invoke-VBox @('modifyvm', $targetUuid, '--uart1', '0x3F8', '4', '--uart-mode1', 'file', $fixedSerial) | Out-Null
		Assert-VMState $m3Vdi | Out-Null
		Invoke-VBox @('modifyvm', $targetUuid, '--uart2', '0x2F8', '3', '--uart-type2', '16550A', '--uart-mode2', 'disconnected') | Out-Null
	} else {
		if (Test-Path -LiteralPath $prepReservation) { throw "M3 reservation exists: $prepReservation" }
		if (Test-Path -LiteralPath $m3Dir) { throw "M3 run directory exists: $m3Dir" }
		if ((Get-Item -LiteralPath $m3ImageSrc -Force).Length -ne $m3ImageSize) { throw 'M3 image size mismatch' }
		if ((Get-FileHash -LiteralPath $m3ImageSrc -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m3ImageSha) { throw 'M3 image SHA-256 mismatch' }
		$info = Assert-VMState $m2Vdi
		if ($info['VMState'] -ne 'poweroff') { throw "VM must be poweroff before preparation: $($info['VMState'])" }
		if (-not $info.ContainsKey('uart2') -or $info['uart2'] -ne 'off') { throw ('COM2 must be off before this experiment: ' + [string]$info['uart2']) }
		Write-NewFile -Path $prepReservation -Text (ConvertTo-Json -Compress ([ordered]@{
			run_id = $targetRun; image_sha256 = $m3ImageSha; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
		$reservationOwned = $true
		New-Item -ItemType Directory -Path $m3Dir | Out-Null
		[IO.File]::Copy($m3ImageSrc, $m3Image, $false)
		if ((Get-FileHash -LiteralPath $m3Image -Algorithm SHA256).Hash.ToLowerInvariant() -ne $m3ImageSha) { throw 'copied M3 image hash mismatch' }
		Assert-NoReparseChain @($m3Image, $m3Vdi, $serialPath, $m3Meta)
		Assert-VMState $m2Vdi | Out-Null
		Invoke-VBox @('convertfromraw', $m3Image, $m3Vdi, '--format', 'VDI') | Out-Null
		Assert-VMState $m2Vdi | Out-Null
		Invoke-VBox @('storageattach', $targetUuid, '--storagectl', 'SATA', '--port', '0', '--device', '0', '--type', 'hdd', '--medium', $m3Vdi) | Out-Null
		$attached = $true
		$info = Assert-VMState $m3Vdi
		if ($info['VMState'] -ne 'poweroff') { throw "VM changed state during preparation: $($info['VMState'])" }
		Invoke-VBox @('modifyvm', $targetUuid, '--uart1', '0x3F8', '4', '--uart-mode1', 'file', $serialPath) | Out-Null
		Assert-VMState $m3Vdi | Out-Null
		Invoke-VBox @('modifyvm', $targetUuid, '--uart2', '0x2F8', '3', '--uart-type2', '16550A', '--uart-mode2', 'disconnected') | Out-Null
		Write-NewFile -Path $m3Meta -Text (ConvertTo-Json -Depth 3 ([ordered]@{
			schema = 'm3-transport'; run_id = $targetRun; vm_uuid = $targetUuid; vm_name = $targetName
			original_owner_sha256 = $originalOwnerSha; m2_image_sha256 = $m2ImageSha; m2_vdi = $m2Vdi
			m3_image_sha256 = $m3ImageSha; m3_raw_path = $m3Image; m3_vdi_path = $m3Vdi
			com1_path = $serialPath; com2_pipe = $pipeName; created_at = (Get-Date).ToUniversalTime().ToString('o') }))
	}
	$info = Assert-VMState $m3Vdi
	if ($info['uart2'] -ne '0x02f8,3' -or $info['uarttype2'] -ne '16550A') { throw 'COM2 hardware configuration mismatch' }
	if ($info['VMState'] -ne 'poweroff') { throw "VM must be poweroff before start: $($info['VMState'])" }
	Assert-Com2Disconnected $info

	# COM2 disconnected 상태로 먼저 부팅: EFI 출력과 Hello가 pipe로 가지 않는다.
	Invoke-VBox @('startvm', $targetUuid, '--type', 'headless') | Out-Null
	$info = Assert-VMState $m3Vdi
	if ($info['VMState'] -ne 'running') { throw "VM not running after start: $($info['VMState'])" }

	$p1Report = Join-Path $collectDir 'phase1.json'
	$p1Out = Join-Path $collectDir 'phase1.stdout.log'
	$p1Err = Join-Path $collectDir 'phase1.stderr.log'
	$flow1 = Invoke-PhaseFlow -Phase 1 -SerialFile $serialPath -HostReport $p1Report -HostOut $p1Out -HostErr $p1Err `
		-ReadSerial { param($p) Get-SerialText $p } `
		-StartHost { param($r, $o, $e) Start-HostProbe -Report $r -Stdout $o -Stderr $e } `
		-WaitReady { param($o) Wait-PipeReady -Path $o -TimeoutMs 10000 } `
		-CheckVM { param($p) $i = Assert-VMState $m3Vdi; if ($i['VMState'] -ne 'running') { throw 'VM not running before phase1 connect' }; if ($i['uart2'] -ne '0x02f8,3' -or $i['uarttype2'] -ne '16550A') { throw 'COM2 hardware changed' } } `
		-ConnectUart { param($p) Invoke-VBox @('controlvm', $targetUuid, 'changeuartmode2', 'client', $pipeName) | Out-Null } `
		-WaitExit { param($p) $p.WaitForExit(155000) } `
		-ValidateReport { param($r, $ph) Assert-PhaseReport -Path $r -Phase $ph }
	$phase1Proc = $flow1.Process
	$phase1Proc.Refresh()
	$phase1Exit = $phase1Proc.ExitCode
	if ($null -eq $phase1Exit) { throw 'phase1 exit code unavailable' }
	$j1 = $flow1.Report
	if ($phase1Exit -ne 0) { throw "phase1 host exit $phase1Exit" }
	$phase1Complete = $true
	$info = Assert-VMState $m3Vdi
	$stateAfterPhase1 = $info['VMState']
	if ($stateAfterPhase1 -ne 'running') { throw "VM not running after phase1: $stateAfterPhase1" }

	# phase1 성공 뒤에만: 기존 client를 끊고 guest가 phase2 프로세스를 시작할 때까지 대기.
	Invoke-VBox @('controlvm', $targetUuid, 'changeuartmode2', 'disconnected') | Out-Null
	$p2Report = Join-Path $collectDir 'phase2.json'
	$p2Out = Join-Path $collectDir 'phase2.stdout.log'
	$p2Err = Join-Path $collectDir 'phase2.stderr.log'
	$flow2 = Invoke-PhaseFlow -Phase 2 -SerialFile $serialPath -HostReport $p2Report -HostOut $p2Out -HostErr $p2Err `
		-ReadSerial { param($p) Get-SerialText $p } `
		-StartHost { param($r, $o, $e) Start-HostProbe -Report $r -Stdout $o -Stderr $e } `
		-WaitReady { param($o) Wait-PipeReady -Path $o -TimeoutMs 10000 } `
		-CheckVM { param($p) $i = Assert-VMState $m3Vdi; if ($i['VMState'] -ne 'running') { throw 'VM not running before phase2 connect' }; if ($i['uart2'] -ne '0x02f8,3' -or $i['uarttype2'] -ne '16550A') { throw 'COM2 hardware changed' } } `
		-ConnectUart { param($p) Invoke-VBox @('controlvm', $targetUuid, 'changeuartmode2', 'client', $pipeName) | Out-Null } `
		-WaitExit { param($p) $p.WaitForExit(155000) } `
		-ValidateReport { param($r, $ph) Assert-PhaseReport -Path $r -Phase $ph }
	$phase2Proc = $flow2.Process
	$phase2Proc.Refresh()
	$phase2Exit = $phase2Proc.ExitCode
	if ($null -eq $phase2Exit) { throw 'phase2 exit code unavailable' }
	$j2 = $flow2.Report
	if ($phase2Exit -ne 0) { throw "phase2 host exit $phase2Exit" }
	$phase2Complete = $true
	$info = Assert-VMState $m3Vdi
	$stateAfterPhase2 = $info['VMState']
	if ($stateAfterPhase2 -ne 'running') { throw "VM not running after phase2: $stateAfterPhase2" }
} catch {
	$prepError = $_.Exception.Message
	Write-Host ('ERROR: ' + $prepError)
} finally {
	Stop-OwnProcess $phase1Proc
	Stop-OwnProcess $phase2Proc
	if ($null -ne $script:CleanupProcError -and $null -eq $cleanupError) { $cleanupError = $script:CleanupProcError; $cleanup.error = $cleanupError }
	if ($reservationOwned -and $null -ne $script:VBox) {
		$markerMissing = @()
		if ($attached) {
			try {
				$markerMissing = Test-SerialMarkers $serialPath
				if ($markerMissing.Count -gt 0) { $serialError = 'COM1 marker missing: ' + ($markerMissing -join ', ') }
			} catch {
				$serialError = 'COM1 marker read failed: ' + $_.Exception.Message
			}
		}
		try {
			$expected = $m2Vdi
			if ($attached) { $expected = $m3Vdi }
			$info = Assert-VMState $expected
			$cleanup.final_state = $info['VMState']
			if ($attached -and ($info['VMState'] -eq 'running' -or $info['VMState'] -eq 'paused')) {
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
			}
			if ($cleanup.final_state -ne 'poweroff') { throw ('final state is not poweroff: {0}' -f $cleanup.final_state) }
			if ($cleanup.action -ne 'poweroff') { throw ('final poweroff not performed; state={0}' -f $cleanup.final_state) }
			if ($null -eq $cleanup.exit_code -or $cleanup.exit_code -ne 0) { throw ('final poweroff exit unavailable/nonzero: {0}' -f $cleanup.exit_code) }
			$cleanup.verified = $true
		} catch {
			$cleanupError = $_.Exception.Message
			$cleanup.error = $cleanupError
			$cleanup.verified = $false
			Write-Diag ('CLEANUP ERROR: ' + $cleanupError)
		}
	}
	$artifacts = @()
	if ($collectReady) {
		$startupArg = ''
		if (Test-Path -LiteralPath $startupRecord -PathType Leaf) { $startupArg = $startupRecord }
		$collectPairs = New-CollectionPairs -Owner $ownerPath -M2 $metaPath -M3 $m3Meta -Serial $serialPath -Startup $startupArg -VBox $vboxLog
		foreach ($pair in $collectPairs) {
			$err = Copy-Fixed -Source $pair.Source -Name $pair.Name
			if ($null -ne $err) { $artifacts += $err }
		}
		if (-not (Test-Path -LiteralPath $nativeLog)) { $artifacts += 'native-test.log: missing' }
		if (-not (Test-Path -LiteralPath $diagPath)) { $artifacts += 'commands.log: missing' }
	}
	if ($artifacts.Count -gt 0) { $artifactError = ($artifacts -join '; ') }
	$measurementComplete = ($null -eq $prepError -and $null -eq $cleanupError -and $null -eq $serialError -and $null -eq $artifactError -and
		$null -eq $script:DiagError -and $nativeExit -eq 0 -and $phase1Exit -eq 0 -and $phase2Exit -eq 0 -and
		$phase1Complete -and $phase2Complete -and $stateAfterPhase1 -eq 'running' -and
		$stateAfterPhase2 -eq 'running' -and $cleanup.verified -and $cleanup.final_state -eq 'poweroff')
	$result = [ordered]@{
		run_id = $targetRun; vm_uuid = $targetUuid; m3_dir = $m3Dir; image_sha256 = $m3ImageSha
		mode = $(if ($ResumePrepared) { 'resume-startup-fix' } else { 'fresh' })
		serial_path = $serialPath; startup_record = $startupRecord
		native_test_exit = $nativeExit; phase1_exit = $phase1Exit; phase2_exit = $phase2Exit
		phase1_measurement_complete = $phase1Complete; phase2_measurement_complete = $phase2Complete
		state_after_phase1 = $stateAfterPhase1; state_after_phase2 = $stateAfterPhase2
		measurement_complete = $measurementComplete; m3_accepted = $false
		abrupt_recovery = 'NOT RUN'; prep_error = $prepError; cleanup = $cleanup
		phase1_report = $p1Report; phase2_report = $p2Report; serial_error = $serialError
		diagnostics_error = $script:DiagError; artifact_error = $artifactError }
	$resultText = ConvertTo-Json -Depth 6 $result
	$writeError = $null
	if ($collectReady) {
		try { Write-NewFile -Path (Join-Path $collectDir 'result.json') -Text $resultText }
		catch { $writeError = $_.Exception.Message }
	}
	if ($null -ne $writeError) {
		[Console]::Error.WriteLine('result write failed: ' + $writeError)
		$measurementComplete = $false
		$result['measurement_complete'] = $false
		$result['artifact_error'] = $writeError
		$resultText = ConvertTo-Json -Depth 6 $result
	}
	if ($collectReady) { Write-Host ('M3 result: ' + (Join-Path $collectDir 'result.json')) }
	Write-Host $resultText
	if (-not $measurementComplete -or $null -ne $writeError) { exit 1 }
}
exit 0
