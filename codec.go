package chains

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Encoding string

const (
	JSON Encoding = "json"
	CBOR Encoding = "cbor"
)

var cborMarker = []byte{0xd9, 0xd9, 0xf7}

func Encode(m Message, encoding Encoding) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	switch encoding {
	case JSON:
		return raw, nil
	case CBOR:
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&value); err != nil {
			return nil, err
		}
		buf := bytes.NewBuffer(bytes.Clone(cborMarker))
		err = writeCBOR(buf, value, 0)
		return buf.Bytes(), err
	default:
		return nil, fmt.Errorf("unknown encoding %q", encoding)
	}
}

func Decode(raw []byte) (Message, error) {
	var m Message
	if bytes.HasPrefix(raw, cborMarker) {
		r := bytes.NewReader(raw[len(cborMarker):])
		value, err := readCBOR(r, 0)
		if err != nil {
			return m, err
		}
		if r.Len() != 0 {
			return m, errors.New("trailing CBOR data")
		}
		raw, err = json.Marshal(value)
		if err != nil {
			return m, err
		}
	}
	err := json.Unmarshal(raw, &m)
	return m, err
}

func head(b *bytes.Buffer, major byte, n uint64) {
	switch {
	case n < 24:
		b.WriteByte(major<<5 | byte(n))
	case n <= math.MaxUint8:
		b.WriteByte(major<<5 | 24)
		b.WriteByte(byte(n))
	case n <= math.MaxUint16:
		b.WriteByte(major<<5 | 25)
		b.Write(binary.BigEndian.AppendUint16(nil, uint16(n)))
	case n <= math.MaxUint32:
		b.WriteByte(major<<5 | 26)
		b.Write(binary.BigEndian.AppendUint32(nil, uint32(n)))
	default:
		b.WriteByte(major<<5 | 27)
		b.Write(binary.BigEndian.AppendUint64(nil, n))
	}
}

func writeCBOR(b *bytes.Buffer, v any, depth int) error {
	if depth > 64 {
		return errors.New("CBOR nesting limit")
	}
	switch x := v.(type) {
	case nil:
		b.WriteByte(0xf6)
	case bool:
		if x {
			b.WriteByte(0xf5)
		} else {
			b.WriteByte(0xf4)
		}
	case string:
		head(b, 3, uint64(len(x)))
		b.WriteString(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			if n < 0 {
				head(b, 1, uint64(-(n + 1)))
			} else {
				head(b, 0, uint64(n))
			}
		} else if n, err := strconv.ParseUint(string(x), 10, 64); err == nil {
			head(b, 0, n)
		} else {
			if !strings.ContainsAny(string(x), ".eE") {
				return errors.New("CBOR integer is outside the supported range")
			}
			f, err := x.Float64()
			if err != nil {
				return err
			}
			b.WriteByte(0xfb)
			b.Write(binary.BigEndian.AppendUint64(nil, math.Float64bits(f)))
		}
	case []any:
		head(b, 4, uint64(len(x)))
		for _, item := range x {
			if err := writeCBOR(b, item, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		head(b, 5, uint64(len(x)))
		for k, item := range x {
			head(b, 3, uint64(len(k)))
			b.WriteString(k)
			if err := writeCBOR(b, item, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unsupported CBOR value")
	}
	return nil
}

func readCBOR(r *bytes.Reader, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("CBOR nesting limit")
	}
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	major, additional := first>>5, first&31
	n := uint64(additional)
	if additional >= 24 {
		if additional > 27 {
			return nil, errors.New("unsupported CBOR length")
		}
		size := 1 << int(additional-24)
		var b [8]byte
		if _, err = io.ReadFull(r, b[8-size:]); err != nil {
			return nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	switch major {
	case 0:
		return n, nil
	case 1:
		if n > math.MaxInt64 {
			return nil, errors.New("CBOR negative integer out of range")
		}
		return -1 - int64(n), nil
	case 3:
		if n > uint64(r.Len()) {
			return nil, io.ErrUnexpectedEOF
		}
		b := make([]byte, int(n))
		_, _ = io.ReadFull(r, b)
		if !utf8.Valid(b) {
			return nil, errors.New("invalid CBOR text")
		}
		return string(b), nil
	case 4, 5:
		if n > uint64(r.Len()) {
			return nil, io.ErrUnexpectedEOF
		}
		if major == 4 {
			a := make([]any, int(n))
			for i := range a {
				a[i], err = readCBOR(r, depth+1)
				if err != nil {
					return nil, err
				}
			}
			return a, nil
		}
		m := make(map[string]any)
		for i := uint64(0); i < n; i++ {
			k, err := readCBOR(r, depth+1)
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, errors.New("expected CBOR text key")
			}
			if _, ok = m[key]; ok {
				return nil, errors.New("duplicate CBOR text key")
			}
			m[key], err = readCBOR(r, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return m, nil
	case 7:
		switch additional {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22:
			return nil, nil
		case 25:
			return halfFloat(uint16(n)), nil
		case 26:
			return math.Float32frombits(uint32(n)), nil
		case 27:
			return math.Float64frombits(n), nil
		}
	}
	return nil, errors.New("unsupported CBOR value")
}

func halfFloat(bits uint16) float64 {
	sign := 1.0
	if bits&0x8000 != 0 {
		sign = -1
	}
	exponent := int((bits >> 10) & 31)
	fraction := float64(bits & 1023)
	switch exponent {
	case 0:
		return sign * math.Ldexp(fraction, -24)
	case 31:
		if fraction != 0 {
			return math.NaN()
		}
		return sign * math.Inf(1)
	default:
		return sign * math.Ldexp(1024+fraction, exponent-25)
	}
}
