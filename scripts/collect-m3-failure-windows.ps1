# M3 phase1 FAIL 증거 회수 전용 — 읽기/복사만. VM 관리 명령/재실행/삭제/설치/네트워크 없음.
# 사용법: powershell -File collect-m3-failure-windows.ps1 [-SelfTest]
param([switch]$SelfTest)
$ErrorActionPreference = 'Stop'
$targetUuid = '8173a191-47a9-497f-a254-5873a31cef0d'
$targetName = 'vboxdistro-m1-3487a95a'
$targetRun = '3487a95a-a207-463b-8a4b-22769e50906f'
$originalOwnerSha = '0113a97fa36bcd44e60f34b4753f20787c747d16ad7bd5b98b1cdcffa0d7d143'
$expectedM3ImageSha = '46bcc05953b26440f664c9385305c368f6584d792479e4031694cbc413dd8963'

$local = $env:LOCALAPPDATA
$runDir = Join-Path (Join-Path $local 'vboxdistro\m1-test') $targetRun
$m3Dir = Join-Path $runDir 'm3-transport-r1'
$ownerPath = Join-Path $runDir 'owner.json'
$metaPath = Join-Path $m3Dir 'm3.json'
$expectedCfg = Join-Path (Join-Path $runDir $targetName) ($targetName + '.vbox')
$vmDir = Join-Path $runDir $targetName

# 고정 object 목록만 쓴다. nested array/string flattening 금지.
$targets = @(
	[pscustomobject]@{ Source = (Join-Path $m3Dir 'serial-console.log'); Name = 'serial-console.log' },
	[pscustomobject]@{ Source = (Join-Path (Join-Path $vmDir 'Logs') 'VBox.log'); Name = 'VBox.log' },
	[pscustomobject]@{ Source = (Join-Path $vmDir ($targetName + '.vbox')); Name = 'current-VM.vbox' }
)

# default/SelfTest 공통: 고정 3개 object의 Source/Name이 flatten되지 않았는지 1회 확인.
$expectedNames = @('serial-console.log', 'VBox.log', 'current-VM.vbox')
if ($targets.Count -ne 3) { throw 'target list must have exactly 3 entries' }
for ($i = 0; $i -lt 3; $i++) {
	$t = $targets[$i]
	if ($t -is [string] -or [string]::IsNullOrEmpty([string]$t.Source) -or [string]::IsNullOrEmpty([string]$t.Name)) { throw 'target list is flattened' }
	if ($t.Name -ne $expectedNames[$i]) { throw ('target name mismatch: ' + [string]$t.Name) }
}

function Test-NoReparse {
	param([string]$Path)
	if (-not (Test-Path -LiteralPath $Path)) { return $true }
	$item = Get-Item -LiteralPath $Path -Force
	return (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0)
}
function Assert-NoReparseChain {
	param([string[]]$Paths)
	foreach ($candidate in $Paths) {
		$probe = [IO.Path]::GetFullPath($candidate)
		while ($true) {
			if (-not (Test-NoReparse $probe)) { throw "reparse point in path chain: $probe" }
			$parent = [IO.Path]::GetDirectoryName($probe.TrimEnd('\'))
			if ([string]::IsNullOrEmpty($parent) -or $parent -eq $probe) { break }
			$probe = $parent
		}
	}
}
function Get-SharedHash {
	param([string]$Path)
	$share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
	$stream = New-Object IO.FileStream($Path, [IO.FileMode]::Open, [IO.FileAccess]::Read, $share)
	try {
		$sha = [Security.Cryptography.SHA256]::Create()
		try { return ([BitConverter]::ToString($sha.ComputeHash($stream))).Replace('-', '').ToLowerInvariant() }
		finally { $sha.Dispose() }
	} finally { $stream.Dispose() }
}
function Copy-SharedToNew {
	param([string]$Source, [string]$Dest)
	$share = [IO.FileShare]::ReadWrite -bor [IO.FileShare]::Delete
	$input = New-Object IO.FileStream($Source, [IO.FileMode]::Open, [IO.FileAccess]::Read, $share)
	try {
		$output = New-Object IO.FileStream($Dest, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
		try { $input.CopyTo($output) } finally { $output.Dispose() }
	} finally { $input.Dispose() }
}
function Write-NewFile {
	param([string]$Path, [string]$Text)
	$bytes = [Text.Encoding]::UTF8.GetBytes($Text)
	$stream = [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
	try { $stream.Write($bytes, 0, $bytes.Length) } finally { $stream.Dispose() }
}

if ($SelfTest) {
	# 실제 과거 runner pattern: 초기 3 nested pairs 뒤에 single @(...) 두 번을 += 해
	# 마지막 4개가 [path, name, path, name]으로 flatten되고, 그 [1] 문자가
	# destination ':', 'e', ':', 'B'가 되는 신호를 메모리에서 재현한다.
	$fixture = 'C:\fixture\m1-test\3487a95a-a207-463b-8a4b-22769e50906f\m3-transport-r1'
	$pairs = @(@((Join-Path $fixture 'owner.json'), 'owner.json'),
		@((Join-Path $fixture 'm2.json'), 'm2.json'),
		@((Join-Path $fixture 'm3.json'), 'm3.json'))
	$pairs += @((Join-Path $fixture 'serial-console.log'), 'serial-console.log')
	$pairs += @((Join-Path $fixture 'Logs\VBox.log'), 'VBox.log')
	$tail = @($pairs[$pairs.Count - 4], $pairs[$pairs.Count - 3], $pairs[$pairs.Count - 2], $pairs[$pairs.Count - 1])
	$signal = ''
	foreach ($entry in $tail) {
		$text = [string]$entry
		$signal += [string]$text[1]
	}
	if ($signal -ne ':e:B') { throw ('flatten fixture did not reproduce char destinations: ' + $signal) }
	Write-Host ('SELFTEST PASS chars=' + $signal + ' tailCount=' + $tail.Count + ' targets=' + $targets.Count)
	exit 0
}

if ([string]::IsNullOrEmpty($local) -or -not [IO.Path]::IsPathRooted($local)) { throw 'LOCALAPPDATA must be an absolute path' }
Assert-NoReparseChain @($local, $runDir, $m3Dir, $ownerPath, $metaPath, $expectedCfg, $vmDir, $PSScriptRoot, $targets[0].Source, $targets[1].Source, $targets[2].Source)

$owner = Get-Content -LiteralPath $ownerPath -Raw | ConvertFrom-Json
if ($owner.run_id -ne $targetRun -or $owner.vm_uuid -ne $targetUuid -or $owner.vm_name -ne $targetName) { throw 'owner identity mismatch' }
if (-not [string]::Equals([IO.Path]::GetFullPath($owner.base_folder), [IO.Path]::GetFullPath($runDir), 'OrdinalIgnoreCase')) { throw 'owner base_folder mismatch' }
if (-not [string]::Equals([IO.Path]::GetFullPath($owner.config_file), [IO.Path]::GetFullPath($expectedCfg), 'OrdinalIgnoreCase')) { throw 'owner config mismatch' }
if ($owner.source_image_sha256 -ne $originalOwnerSha) { throw 'owner source hash mismatch' }

$meta = Get-Content -LiteralPath $metaPath -Raw | ConvertFrom-Json
if ($meta.schema -ne 'm3-transport' -or $meta.run_id -ne $targetRun -or $meta.vm_uuid -ne $targetUuid -or $meta.vm_name -ne $targetName) { throw 'm3.json identity mismatch' }
if ($meta.original_owner_sha256 -ne $originalOwnerSha -or $meta.m3_image_sha256 -ne $expectedM3ImageSha) { throw 'm3.json hash binding mismatch' }
foreach ($p in @([string]$meta.m3_raw_path, [string]$meta.m3_vdi_path)) {
	$full = [IO.Path]::GetFullPath($p)
	$prefix = [IO.Path]::GetFullPath($m3Dir) + [IO.Path]::DirectorySeparatorChar
	if (-not $full.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) { throw ('m3.json path outside m3 dir: ' + $p) }
}

$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
$resultName = 'result-m3-diagnostics-' + $stamp + '-' + ([Guid]::NewGuid().ToString('N').Substring(0, 8))
$resultDir = Join-Path $PSScriptRoot $resultName
New-Item -ItemType Directory -Path $resultDir | Out-Null
Assert-NoReparseChain @($resultDir)

$records = New-Object System.Collections.Generic.List[object]
$errors = New-Object System.Collections.Generic.List[string]
$copiedCount = 0
foreach ($target in $targets) {
	$record = [pscustomobject]@{
		Name = $target.Name; Source = $target.Source; Bytes = $null
		SHA256 = $null; SourceSHA256Before = $null; SourceSHA256After = $null
		SourceUnchanged = $false; Copied = $false; Error = $null
	}
	try {
		if (-not (Test-Path -LiteralPath $target.Source -PathType Leaf)) { throw 'source missing' }
		$before = Get-SharedHash $target.Source
		$dest = Join-Path $resultDir $target.Name
		Copy-SharedToNew -Source $target.Source -Dest $dest
		$srcAfter = Get-SharedHash $target.Source
		$destHash = Get-SharedHash $dest
		$srcLenAfter = (Get-Item -LiteralPath $target.Source -Force).Length
		$destLen = (Get-Item -LiteralPath $dest -Force).Length
		$record.Bytes = $destLen
		$record.SHA256 = $destHash
		$record.SourceSHA256Before = $before
		$record.SourceSHA256After = $srcAfter
		$record.SourceUnchanged = ($before -eq $srcAfter)
		$record.Copied = ($before -eq $srcAfter) -and ($before -eq $destHash) -and ($destLen -eq $srcLenAfter)
		if (-not $record.SourceUnchanged) { throw 'source changed during copy' }
		if (-not $record.Copied) { throw 'destination hash/size mismatch' }
		$copiedCount++
	} catch {
		$record.Error = $_.Exception.Message
		$errors.Add(($target.Name + ': ' + $record.Error))
	}
	$records.Add($record)
}

$collection = [pscustomobject]@{
	run_id = $targetRun; vm_uuid = $targetUuid; vm_name = $targetName
	collected_at = (Get-Date).ToUniversalTime().ToString('o')
	expected_image_sha256 = $expectedM3ImageSha
	result_dir = $resultDir
	files = $records
	errors = $errors
	all_copied = ($copiedCount -eq $targets.Count)
}
$json = ConvertTo-Json -Depth 6 $collection
Write-NewFile -Path (Join-Path $resultDir 'collection.json') -Text $json
Write-Host ('M3 diagnostics result: ' + (Join-Path $resultDir 'collection.json'))
Write-Host $json
if ($copiedCount -ne $targets.Count) { exit 1 }
exit 0
