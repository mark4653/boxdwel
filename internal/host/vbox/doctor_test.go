//go:build linux

package vbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fake(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "VBoxManage")
	// The fake also rejects any command other than the read-only version query.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = --version ] || exit 99\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfig(t *testing.T, path, executable string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"vboxmanage_path": executable})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"supported", "echo 7.2.4r12345", ""},
		{"plain version", "echo 7.2.0", ""},
		{"unsupported", "echo 7.1.8r12345", "unsupported"},
		{"malformed", "echo garbage", "unsupported"},
		{"empty", "exit 0", "unsupported"},
		{"exit failure", "echo probe-failed >&2; exit 23", "exit status 23: probe-failed"},
		{"timeout", "while :; do :; done", "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := fake(t, dir, tc.body)
			config := filepath.Join(dir, "config.json")
			writeConfig(t, config, path)
			result, err := Detect(config, nil, 100*time.Millisecond)
			if result.Path != path {
				t.Fatalf("path = %q", result.Path)
			}
			if tc.want == "" {
				if err != nil || !strings.HasPrefix(result.Version, "7.2.") {
					t.Fatalf("result=%+v error=%v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestDiscoveryPrecedence(t *testing.T) {
	dir := t.TempDir()
	fromPath := fake(t, dir, "echo 7.2.1")
	t.Setenv("PATH", dir)
	defaultPath := fake(t, t.TempDir(), "echo 7.2.2")
	config := filepath.Join(t.TempDir(), "config.json")
	result, err := Detect(config, []string{defaultPath}, time.Second)
	if err != nil || result.Path != fromPath {
		t.Fatalf("PATH precedence: %+v %v", result, err)
	}
	writeConfig(t, config, defaultPath)
	before, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Detect(config, nil, time.Second)
	if err != nil || result.Path != defaultPath {
		t.Fatalf("config precedence: %+v %v", result, err)
	}
	after, err := os.ReadFile(config)
	if err != nil || string(before) != string(after) {
		t.Fatalf("doctor changed config: %v", err)
	}
	if err := os.WriteFile(config, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = Detect(config, nil, time.Second)
	if err != nil || result.Path != fromPath {
		t.Fatalf("empty config discovery: %+v %v", result, err)
	}
	t.Setenv("PATH", t.TempDir())
	missingConfig := filepath.Join(t.TempDir(), "absent.json")
	result, err = Detect(missingConfig, []string{defaultPath}, time.Second)
	if err != nil || result.Path != defaultPath {
		t.Fatalf("default discovery: %+v %v", result, err)
	}
	if _, err := Detect(missingConfig, nil, time.Second); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing executable: %v", err)
	}
	writeConfig(t, config, filepath.Join(dir, "missing"))
	if _, err := Detect(config, []string{defaultPath}, time.Second); err == nil {
		t.Fatal("invalid explicit path must not fall back")
	}
}

func TestRejectNonExactConfigKeysWithValidPATH(t *testing.T) {
	dir := t.TempDir()
	fromPath := fake(t, dir, "echo 7.2.1")
	t.Setenv("PATH", dir)
	config := filepath.Join(t.TempDir(), "config.json")
	result, err := Detect(config, nil, time.Second)
	if err != nil || result.Path != fromPath {
		t.Fatalf("PATH fake must work: %+v %v", result, err)
	}
	for _, key := range []string{"VBOXMANAGE_PATH", "VBoxManage_Path", "unknown"} {
		t.Run(key, func(t *testing.T) {
			data, err := json.Marshal(map[string]string{key: filepath.Join(dir, "missing")})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, data, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Detect(config, nil, time.Second)
			if err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("non-exact key %q must be rejected, not fall back: result=%+v error=%v", key, result, err)
			}
		})
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, data := range []string{`{`, `null`, `[]`, `{"unknown":1}`, `{} {}`, `{"vboxmanage_path":null}`, `{"vboxmanage_path":""}`, `{"vboxmanage_path":"relative"}`, `{"vboxmanage_path":42}`} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Detect(path, nil, time.Second); err == nil || !strings.Contains(err.Error(), "config") {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}
	if _, err := Detect(t.TempDir(), nil, time.Second); err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("read failure: %v", err)
	}
}
