//go:build windows

package main

import (
	"io"

	"github.com/mark4653/boxdwel/internal/enter"
)

func runEnter(stdout io.Writer) error { return enter.Run(stdout) }
