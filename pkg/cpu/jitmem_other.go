//go:build !unix

package cpu

import "errors"

func mapExecutable(size int) ([]byte, error) { return nil, errors.New("not supported on this system") }
