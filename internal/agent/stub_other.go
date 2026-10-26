//go:build !linux

// Package agent implements the Linux guest agent. Other systems build an
// empty package so cross-compilation and `go test ./...` still work.
package agent
