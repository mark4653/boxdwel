//go:build windows

// Command m3-transport-probe is the Windows host side of the M3 COM2 transport
// spike. It creates the fixed pipe, waits for the VM COM2 client, runs one
// bounded measurement, and writes a JSON report. No VBoxManage command runs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mark4653/boxdwel/internal/enter"
	"github.com/mark4653/boxdwel/internal/m3probe"
)

type envelope struct {
	Protocol            string         `json:"protocol"`
	MeasurementComplete bool           `json:"measurement_complete"`
	Error               string         `json:"error"`
	Report              m3probe.Report `json:"report"`
}

func main() { os.Exit(run()) }

func run() int {
	report := flag.String("report", "", "absolute path for the JSON report (must not exist)")
	flag.Parse()
	if *report == "" || !filepath.IsAbs(*report) {
		fmt.Fprintln(os.Stderr, "m3probe-host: -report requires an absolute path")
		return 2
	}
	f, err := os.OpenFile(*report, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "m3probe-host: report: %v\n", err)
		return 1
	}
	rep, runErr := enter.RunM3TransportProbe(os.Stdout)
	env := envelope{Protocol: "m3-transport-spike-v1", MeasurementComplete: runErr == nil, Report: rep}
	if runErr != nil {
		env.Error = runErr.Error()
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "m3probe-host: report encode: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "m3probe-host: report close: %v\n", err)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "m3probe-host: %v\n", runErr)
		return 1
	}
	fmt.Fprintln(os.Stderr, "m3probe-host: measurement complete")
	return 0
}
