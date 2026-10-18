//go:build !unix

package d3d9

import (
	"errors"
	"os"
)

func mapShm(f *os.File, size int) ([]byte, error) {
	return nil, errors.New("shared memory viewer needs a unix host")
}
