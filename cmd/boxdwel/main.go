package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mark4653/boxdwel/internal/buildinfo"
	"github.com/mark4653/boxdwel/internal/host/vbox"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		if err := json.NewEncoder(stdout).Encode(struct {
			Version  string  `json:"version"`
			Commit   *string `json:"commit"`
			Protocol *string `json:"protocol_version"`
		}{Version: buildinfo.Version, Commit: buildinfo.CommitSHA()}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case len(args) == 1 && args[0] == "doctor":
		return runDoctor(stdout, stderr)
	case len(args) == 2 && args[0] == "enter" && args[1] == "test":
		if err := runEnter(stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	default:
		fmt.Fprintln(stderr, "usage: boxdwel version|doctor|enter test")
		return 2
	}
}

func runDoctor(stdout, stderr io.Writer) int {
	report := struct {
		OS           string      `json:"os"`
		Architecture string      `json:"architecture"`
		Config       string      `json:"config_path"`
		VBox         vbox.Result `json:"virtualbox"`
		Errors       []string    `json:"errors"`
		Prerequisite string      `json:"prerequisite"`
	}{OS: runtime.GOOS, Architecture: runtime.GOARCH, Errors: []string{},
		Prerequisite: "Windows/amd64, VirtualBox 7.2.x; actual Windows execution required for enter test; single local named-pipe serial session"}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		report.Errors = append(report.Errors, "unsupported host: require Windows/amd64; Linux execution is diagnostic only")
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" || !filepath.IsAbs(base) {
		report.Errors = append(report.Errors, "LOCALAPPDATA must be an absolute path; cannot resolve user config")
	} else {
		report.Config = filepath.Join(base, "vboxdistro", "config.json")
		programFiles := os.Getenv("ProgramFiles")
		if programFiles == "" {
			programFiles = `C:\Program Files`
		}
		var err error
		report.VBox, err = vbox.Detect(report.Config, []string{filepath.Join(programFiles, "Oracle", "VirtualBox", "VBoxManage.exe")}, 5*time.Second)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, err := range report.Errors {
		fmt.Fprintln(stderr, err)
	}
	if len(report.Errors) != 0 {
		return 1
	}
	return 0
}
