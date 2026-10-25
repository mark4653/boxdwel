// Package vbox probes VBoxManage without changing VirtualBox configuration.
package vbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Result struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Detect gives explicit configuration priority; invalid configuration never falls back.
func Detect(configPath string, defaults []string, timeout time.Duration) (Result, error) {
	var result Result
	data, err := os.ReadFile(configPath)
	if err == nil {
		var config struct {
			Path string `json:"vboxmanage_path"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&config); err != nil {
			return result, fmt.Errorf("config %s: %w", configPath, err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return result, fmt.Errorf("config %s: expected one JSON object", configPath)
		}
		// Empty objects use discovery; explicit empty/null paths are invalid.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
			return result, fmt.Errorf("config %s: expected JSON object", configPath)
		}
		for key := range fields {
			if key != "vboxmanage_path" {
				return result, fmt.Errorf("config %s: unknown field %q", configPath, key)
			}
		}
		if _, exists := fields["vboxmanage_path"]; exists {
			if !filepath.IsAbs(config.Path) {
				return result, fmt.Errorf("config %s: vboxmanage_path must be an absolute executable path", configPath)
			}
			result.Path = config.Path
		}
	} else if !os.IsNotExist(err) {
		return result, fmt.Errorf("read config: %w", err)
	}
	if result.Path == "" {
		result.Path, err = exec.LookPath("VBoxManage")
		if err != nil {
			for _, path := range defaults {
				if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
					result.Path = path
					break
				}
			}
		}
	}
	if result.Path == "" {
		return result, fmt.Errorf("VBoxManage not found: configure vboxmanage_path, PATH, or install VirtualBox 7.2.x")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, result.Path, "--version")
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return result, fmt.Errorf("VBoxManage --version timed out: %w", ctx.Err())
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return result, fmt.Errorf("VBoxManage --version: %w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return result, fmt.Errorf("VBoxManage --version: %w", err)
	}
	result.Version = strings.TrimSpace(string(output))
	if !regexp.MustCompile(`^7\.2\.[0-9]+(?:r[0-9]+)?$`).MatchString(result.Version) {
		return result, fmt.Errorf("unsupported VBoxManage version %q: require 7.2.x", result.Version)
	}
	return result, nil
}
