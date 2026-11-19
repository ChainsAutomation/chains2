//go:build !webui

package webui

import "io/fs"

func Assets() fs.FS { return nil }
