//go:build !windows

package main

import (
	"errors"
	"io"
)

func runEnter(io.Writer) error { return errors.New("enter test requires Windows/amd64") }
