#!/usr/bin/env python3
"""Static safety check for scripts/test-m3-transport-windows.ps1.

Usage: check-m3-runner.py <script> [log]
Text check only; it does not parse or run PowerShell.
"""
import hashlib
import sys


def main():
    path = sys.argv[1]
    raw = open(path, "rb").read()
    text = raw.decode("utf-8-sig")
    checks = []

    def check(name, ok):
        checks.append((name, bool(ok)))

    check("utf8_bom", raw.startswith(b"\xef\xbb\xbf"))
    check("resume_and_selftest_switches", "param([switch]$ResumePrepared, [switch]$SelfTest)" in text)
    check("uart_type2_official", "'--uart-type2', '16550A'" in text)
    check("uarttype2_option_absent", "'--uarttype2'" not in text)
    check("uart_mode2_disconnected", "'--uart-mode2', 'disconnected'" in text)
    check("controlvm_changeuartmode2", "'changeuartmode2', 'client'" in text)
    check("controlvm_disconnect", "'changeuartmode2', 'disconnected'" in text)
    check("com2_requires_off_key", "ContainsKey('uart2')" in text and "$info['uart2'] -ne 'off'" in text)
    check("disconnected_before_startvm", text.index("'--uart-mode2', 'disconnected'") < text.index("'startvm'"))
    check("one_startvm", text.count("'startvm'") == 1)
    check("phase_markers_computed", "'M3_PHASE' + $Phase + '_START'" in text)
    check("guest_marker_before_host", text.index("guest phase marker timeout") < text.index("$proc = & $StartHost"))
    check("phase2_after_phase1", text.index("$phase1Complete = $true") < text.index("$flow2 = Invoke-PhaseFlow"))
    check("phase2_disconnect_after_phase1", text.index("$phase1Complete = $true") < text.index("'changeuartmode2', 'disconnected'"))
    check("startvm_before_phase1_flow", text.index("'startvm'") < text.index("$flow1 = Invoke-PhaseFlow"))
    check("resume_reservation", "m3-transport-r1-startup-fix.reservation" in text)
    check("resume_fixed_serial", "serial-console-startup-fix.log" in text)
    check("resume_prepared_validation", "Assert-M3MetaResume" in text and "prep reservation mismatch" in text)
    check("resume_preserves_original", "preserved-m3.json" in text and "preserved-serial-console.log" in text)
    check("resume_no_image_copy", "$ResumePrepared" in text)
    check("preserve_failure_throws", "preflight preserve failed" in text)
    check("owner_before_preserve", text.index("Assert-Owner") < text.index("preflight preserve failed"))
    check("native_after_reparse", text.index("Assert-NoReparseChain @($hostExe, $nativeExe, $collectDir)") < text.index("Run-Native -Exe $nativeExe"))
    check("m2_meta_reparse", "M2 image/VDI is a reparse point" in text)
    check("current_vdi_chain", "Assert-NoReparseChain @($current)" in text)
    check("handle_pinned", "$null = $proc.Handle" in text)
    check("exit_refresh_null", "phase1 exit code unavailable" in text and "phase2 exit code unavailable" in text)
    check("two_155s_waits", text.count("WaitForExit(155000)") == 2)
    check("two_ready_10s", text.count("TimeoutMs 10000") == 2)
    check("serial_shared_read", "FileShare]::ReadWrite" in text and "[IO.FileShare]::Delete" in text)
    check("marker_read_try_catch", "COM1 marker read failed" in text)
    check("marker_does_not_block_poweroff", text.index("COM1 marker read failed") < text.index("'controlvm' $targetUuid 'poweroff'"))
    check("serial_error_separate", "serial_error = $serialError" in text and "$null -eq $serialError" in text)
    check("serial_three_markers", all(m in text for m in ("M3_PHASE1_EXIT rc=0", "M3_PHASE2_EXIT rc=0", "M3_PROBE_DONE")))
    check("markers_before_poweroff", text.index("Test-SerialMarkers") < text.index("'controlvm' $targetUuid 'poweroff'"))
    check("final_poweroff", "'controlvm' $targetUuid 'poweroff'" in text)
    check("strict_final_poweroff", "final state is not poweroff" in text and "final poweroff exit unavailable/nonzero" in text)
    check("verified_only_after_poweroff", "action -ne 'poweroff'" in text and text.count("$cleanup.verified = $true") == 1)
    check("collection_pairs_objects", "function New-CollectionPairs" in text and "[pscustomobject]@{ Source = $Owner; Name = 'owner.json' }" in text)
    check("no_nested_append", "+= @(@(" not in text)
    check("self_test_present", "SELFTEST PASS" in text and "host started before guest marker" in text)
    check("helper_owns_proc", "$proc = $null" in text and "if ($null -ne $proc) { Stop-OwnProcess $proc }" in text)
    check("marker_read_at_least_once", "do {" in text and "& $ReadSerial $SerialFile" in text and "selfReads" in text)
    check("helper_timeout_kills", "own process was not killed" in text)
    check("com2_disconnected_gate", "function Assert-Com2Disconnected" in text and text.index("Assert-Com2Disconnected $info") < text.index("'startvm'"))
    check("connect_checks_addr_and_type", text.count("$i['uart2'] -ne '0x02f8,3' -or $i['uarttype2'] -ne '16550A'") >= 2)
    check("firmware_not_ready", "phase2 host started on phase1 marker" in text)
    check("result_not_pass_field", "m3_accepted = $false" in text and "threshold_pass" not in text)
    check("result_path_printed", "M3 result: " in text)

    out = []
    for name, ok in checks:
        out.append(("PASS " if ok else "FAIL ") + name)
    ok = all(v for _, v in checks)
    out.append("sha256=" + hashlib.sha256(raw).hexdigest())
    out.append("static_assert=" + ("PASS" if ok else "FAIL"))
    out.append("note=PowerShell parser NOT RUN (static text check only)")
    report = "\n".join(out)
    print(report)
    if len(sys.argv) > 2:
        with open(sys.argv[2], "w") as f:
            f.write(report + "\n")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
