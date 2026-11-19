package keyboard

/*
#cgo pkg-config: libevdev
#include <libevdev/libevdev.h>
#include <linux/input.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

type evdev struct {
	fd      int
	device  *C.struct_libevdev
	names   map[uint16]string
	grabbed bool
}

func openEvdev(path string, grab bool) (Device, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dev := &evdev{fd: fd, names: map[uint16]string{}}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		_ = dev.Close()
		return nil, errors.New("keyboard path is not a character device")
	}
	if rc := C.libevdev_new_from_fd(C.int(fd), &dev.device); rc < 0 {
		_ = dev.Close()
		return nil, unix.Errno(-rc)
	}
	for code := 0; code <= C.KEY_MAX; code++ {
		if C.libevdev_has_event_code(dev.device, C.EV_KEY, C.uint(code)) == 0 {
			continue
		}
		name := C.GoString(C.libevdev_event_code_get_name(C.EV_KEY, C.uint(code)))
		if name == "" {
			name = fmt.Sprintf("KEY_%d", code)
		}
		if strings.HasPrefix(name, "KEY_") && code != 0 {
			dev.names[uint16(code)] = name
		}
	}
	if len(dev.names) == 0 {
		_ = dev.Close()
		return nil, errors.New("input device reports no keyboard keys")
	}
	if grab {
		if rc := C.libevdev_grab(dev.device, C.LIBEVDEV_GRAB); rc < 0 {
			_ = dev.Close()
			return nil, unix.Errno(-rc)
		}
		dev.grabbed = true
	}
	return dev, nil
}

func (d *evdev) Held() []Key {
	var keys []Key
	for code, name := range d.names {
		if C.libevdev_get_event_value(d.device, C.EV_KEY, C.uint(code)) != 0 {
			keys = append(keys, Key{code, name})
		}
	}
	return keys
}

func (d *evdev) Next(ctx context.Context) (Input, error) {
	for ctx.Err() == nil {
		var event C.struct_input_event
		rc := C.libevdev_next_event(d.device, C.LIBEVDEV_READ_FLAG_NORMAL, &event)
		switch rc {
		case C.LIBEVDEV_READ_STATUS_SUCCESS:
			if event._type != C.EV_KEY {
				continue
			}
			code := uint16(event.code)
			if name := d.names[code]; name != "" {
				return Input{Key: Key{code, name}, Value: int32(event.value)}, nil
			}
		case C.LIBEVDEV_READ_STATUS_SYNC:
			for ctx.Err() == nil {
				rc = C.libevdev_next_event(d.device, C.LIBEVDEV_READ_FLAG_SYNC, &event)
				if rc == C.LIBEVDEV_READ_STATUS_SYNC || rc == -C.int(unix.EINTR) {
					continue
				}
				if rc == -C.int(unix.EAGAIN) {
					return Input{Resync: true}, nil
				}
				if rc < 0 {
					return Input{}, unix.Errno(-rc)
				}
			}
		case -C.int(unix.EAGAIN):
			poll := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
			_, err := unix.Poll(poll, 200)
			if err != nil && !errors.Is(err, unix.EINTR) {
				return Input{}, err
			}
			if poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return Input{}, unix.ENODEV
			}
		case -C.int(unix.EINTR):
		default:
			return Input{}, unix.Errno(-rc)
		}
	}
	return Input{}, ctx.Err()
}

func (d *evdev) Close() error {
	if d.device != nil {
		if d.grabbed {
			C.libevdev_grab(d.device, C.LIBEVDEV_UNGRAB)
		}
		C.libevdev_free(d.device)
	}
	return unix.Close(d.fd)
}
