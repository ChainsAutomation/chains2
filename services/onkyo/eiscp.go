package onkyo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"time"
)

const headerSize = 16

type message struct {
	code, value string
	received    time.Time
}

func packet(command string) []byte {
	payload := "!1" + command + "\r"
	data := make([]byte, headerSize+len(payload))
	copy(data, "ISCP")
	binary.BigEndian.PutUint32(data[4:8], headerSize)
	binary.BigEndian.PutUint32(data[8:12], uint32(len(payload)))
	data[12] = 1
	copy(data[headerSize:], payload)
	return data
}

func readPacket(reader io.Reader) ([]message, error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[8:12])
	if string(header[:4]) != "ISCP" || binary.BigEndian.Uint32(header[4:8]) != headerSize ||
		header[12] != 1 || size == 0 || size > 64*1024 {
		return nil, errors.New("invalid eISCP frame header")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	var messages []message
	for _, line := range bytes.FieldsFunc(payload, func(r rune) bool { return r == '\r' || r == '\n' || r == 0x1a }) {
		line := strings.TrimRight(string(line), "\x00")
		if len(line) > 5 && strings.HasPrefix(line, "!1") {
			messages = append(messages, message{code: line[2:5], value: line[5:], received: time.Now()})
		}
	}
	return messages, nil
}
