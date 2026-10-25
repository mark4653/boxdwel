//go:build !windows

package enter

import (
	"errors"
	"io"
)

// Run is unavailable outside Windows; Linux builds are diagnostic only.
func Run(io.Writer) error {
	return errors.New("enter test requires Windows/amd64")
}
