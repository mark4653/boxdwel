//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mark4653/boxdwel/internal/agent"
)

func main() {
	dev := flag.String("dev", "/dev/ttyS1", "serial device for the product protocol")
	flag.Parse()

	conn, err := agent.OpenSerial(*dev)
	if err != nil {
		fmt.Fprintln(os.Stderr, "boxdwel-agent:", err)
		os.Exit(1)
	}
	if err := agent.Serve(conn); err != nil {
		fmt.Fprintln(os.Stderr, "boxdwel-agent:", err)
		os.Exit(1)
	}
}
