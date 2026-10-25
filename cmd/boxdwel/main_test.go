package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
)

func TestVersionAndUsage(t *testing.T) {
	var out, stderr bytes.Buffer
	if rc := run([]string{"version"}, &out, &stderr); rc != 0 || stderr.Len() != 0 {
		t.Fatalf("version: rc=%d stderr=%s", rc, &stderr)
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["version"] != "dev" || report["commit"] != nil || report["protocol_version"] != nil {
		t.Fatalf("unexpected metadata: %v", report)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"version", "extra"}, {"doctor", "extra"}, {"enter"}, {"enter", "other"}, {"enter", "test", "extra"}} {
		out.Reset()
		stderr.Reset()
		if rc := run(args, &out, &stderr); rc != 2 || out.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("usage args=%v rc=%d stdout=%s stderr=%s", args, rc, &out, &stderr)
		}
	}
}

func TestEnterUnsupportedOnNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows runs the console session instead")
	}
	var out, stderr bytes.Buffer
	if rc := run([]string{"enter", "test"}, &out, &stderr); rc != 1 || stderr.Len() == 0 {
		t.Fatalf("enter rc=%d stderr=%s", rc, &stderr)
	}
}

func TestDoctorInvalidEnvironment(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	var out, stderr bytes.Buffer
	if rc := run([]string{"doctor"}, &out, &stderr); rc != 1 || stderr.Len() == 0 {
		t.Fatalf("doctor rc=%d stderr=%s", rc, &stderr)
	}
	var report struct {
		OS     string   `json:"os"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.OS != runtime.GOOS || len(report.Errors) == 0 {
		t.Fatalf("report=%+v error=%v", report, err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func TestOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	if rc := run([]string{"version"}, brokenWriter{}, &stderr); rc != 1 || stderr.Len() == 0 {
		t.Fatalf("output failure rc=%d stderr=%s", rc, &stderr)
	}
}
