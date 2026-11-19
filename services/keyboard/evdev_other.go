//go:build !linux

package keyboard

import "errors"

func openEvdev(string, bool) (Device, error) {
	return nil, errors.New("the USB keyboard service requires Linux evdev")
}
