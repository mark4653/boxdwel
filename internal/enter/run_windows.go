//go:build windows

package enter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark4653/boxdwel/internal/host/vbox"
)

const (
	pipeName    = `\\.\pipe\vboxdistro-m2-console`
	coldTimeout = 90 * time.Second
	warmTimeout = 20 * time.Second
	acceptWait  = 30 * time.Second
	readySlice  = time.Second
)

type vmInfo map[string]string

// Run enters the approved test VM's single BusyBox shell. It never powers the VM off.
func Run(out io.Writer) (err error) {
	p, err := ResolvePaths(os.Getenv("LOCALAPPDATA"))
	if err != nil {
		return err
	}
	_, meta, err := Load(p)
	if err != nil {
		return err
	}
	if err := VerifyImage(meta.ImagePath, meta.ImageSHA); err != nil {
		return err
	}
	exe, err := detectVBox()
	if err != nil {
		return err
	}
	if _, err := checkedState(exe, p, meta); err != nil {
		return err
	}
	// The first-instance pipe is the single-session lock; creation returns
	// immediately and the peer is accepted only after the VM is started/attached.
	pipe, err := createConsolePipe(pipeName)
	if err != nil {
		return err
	}
	defer pipe.Close()
	state, err := checkedState(exe, p, meta)
	if err != nil {
		return err
	}
	switch state {
	case "poweroff":
		if err := revalidate(exe, p, meta); err != nil {
			return err
		}
		if _, err := runVBox(exe, "modifyvm", TargetUUID, "--uart-mode1", "client", pipeName); err != nil {
			return err
		}
		if err := revalidate(exe, p, meta); err != nil {
			return err
		}
		if _, err := runVBox(exe, "startvm", TargetUUID, "--type", "headless"); err != nil {
			return err
		}
	case "running":
		if err := revalidate(exe, p, meta); err != nil {
			return err
		}
		if _, err := runVBox(exe, "controlvm", TargetUUID, "changeuartmode1", "client", pipeName); err != nil {
			return err
		}
	default:
		return fmt.Errorf("test VM state %q is not usable; expected poweroff or running", state)
	}
	if err := pipe.Accept(acceptWait); err != nil {
		return err
	}

	conn := NewConn(pipe)
	defer conn.Close()
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	cmd, err := readyCommand(nonce)
	if err != nil {
		return err
	}
	wait := warmTimeout
	if state == "poweroff" {
		wait = coldTimeout
	}
	deadline := time.Now().Add(wait)
	for {
		if err := conn.Send(cmd); err != nil {
			return err
		}
		slice := readySlice
		if remaining := time.Until(deadline); remaining < slice {
			slice = remaining
		}
		err = conn.WaitLine(ReadyPrefix+nonce, out, slice)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrTimeout) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("M2 shell ready marker not seen within %s: %w", wait, err)
		}
	}
	raw, err := enableRawConsole()
	if err != nil {
		return err
	}
	// shortcut: the stdin copy goroutine may stay blocked until process exit;
	// add cancellable console input before reusing this package in a long-lived host.
	defer func() { err = errors.Join(err, raw.restore()) }()
	return conn.Pump(os.Stdin, out, exitMarker(nonce))
}

func checkedState(exe string, p Paths, meta Meta) (string, error) {
	info, err := showVM(exe, TargetUUID)
	if err != nil {
		return "", err
	}
	return validateVM(info, p, meta)
}

func revalidate(exe string, p Paths, meta Meta) error {
	_, err := checkedState(exe, p, meta)
	return err
}

func detectVBox() (string, error) {
	config := filepath.Join(os.Getenv("LOCALAPPDATA"), "vboxdistro", "config.json")
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	r, err := vbox.Detect(config, []string{filepath.Join(programFiles, "Oracle", "VirtualBox", "VBoxManage.exe")}, 5*time.Second)
	if err != nil {
		return "", err
	}
	return r.Path, nil
}

func validateVM(info vmInfo, p Paths, meta Meta) (string, error) {
	state := info["VMState"]
	if state == "" {
		return "", fmt.Errorf("showvminfo did not report VMState")
	}
	if !strings.EqualFold(info["UUID"], TargetUUID) || info["name"] != TargetName {
		return "", fmt.Errorf("VM identity mismatch: UUID=%q name=%q", info["UUID"], info["name"])
	}
	cfg, err := filepath.Abs(info["CfgFile"])
	if err != nil || !strings.EqualFold(cfg, p.Config) {
		return "", fmt.Errorf("VM config path %q does not match %q", info["CfgFile"], p.Config)
	}
	if info["cpus"] != "1" || info["memory"] != "512" || !strings.EqualFold(info["firmware"], "efi64") {
		return "", fmt.Errorf("VM resources changed: cpus=%s memory=%s firmware=%s", info["cpus"], info["memory"], info["firmware"])
	}
	for i := 1; i <= 8; i++ {
		key := fmt.Sprintf("nic%d", i)
		if !strings.EqualFold(info[key], "none") {
			return "", fmt.Errorf("%s is %q, expected none", key, info[key])
		}
	}
	if !strings.EqualFold(info["uart1"], "0x03f8,4") || !strings.EqualFold(info["uarttype1"], "16550A") {
		return "", fmt.Errorf("UART changed: uart1=%q uarttype1=%q", info["uart1"], info["uarttype1"])
	}
	attached := info["SATA-0-0"]
	if attached == "" {
		return "", fmt.Errorf("SATA port 0 is not attached")
	}
	attachedPath, err := filepath.Abs(attached)
	if err != nil || !strings.EqualFold(attachedPath, meta.VDIPath) {
		return "", fmt.Errorf("SATA port 0 medium %q does not match M2 VDI %q", attached, meta.VDIPath)
	}
	return state, nil
}
