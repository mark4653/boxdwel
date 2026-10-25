//go:build windows

package enter

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func runVBox(exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("VBoxManage %s timed out", strings.Join(args, " "))
	}
	if err != nil {
		return "", fmt.Errorf("VBoxManage %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func showVM(exe, uuid string) (vmInfo, error) {
	out, err := runVBox(exe, "showvminfo", uuid, "--machinereadable")
	if err != nil {
		return nil, err
	}
	info := vmInfo{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.Trim(line[:i], `"`)
		value := line[i+1:]
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = value[1 : len(value)-1]
		}
		value = strings.ReplaceAll(value, `\\`, `\`)
		value = strings.ReplaceAll(value, `\"`, `"`)
		info[key] = value
	}
	return info, nil
}
