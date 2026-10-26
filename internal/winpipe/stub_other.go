//go:build !windows

// Package winpipe is the Windows named-pipe transport. Other systems build an
// empty package so cross-compilation and `go test ./...` still work.
package winpipe
