//go:build !unix

package main

import "errors"

// setUmask is only meaningful on Unix; refuse rather than silently ignore it.
func setUmask(int) error {
	return errors.New("UMASK is only supported on Unix; unset it to run on this OS")
}
