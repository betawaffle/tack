//go:build !amd64

package tools

import "errors"

// codeFilter refuses, because code doesn't run without a filter and the list of syscalls
// has only been written for x86-64.
func codeFilter() ([]byte, error) {
	return nil, errors.New("running code is only set up for x86-64")
}
