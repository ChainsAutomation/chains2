package chains

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestWireFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/event.json")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := expected.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	binaryFixture, err := os.ReadFile("testdata/event.cbor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(binaryFixture); err != nil {
		t.Fatal(err)
	}
	for _, encoding := range []Encoding{JSON, CBOR} {
		wire, err := Encode(expected, encoding)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := Decode(wire)
		if err != nil {
			t.Fatal(err)
		}
		var left, right any
		if err = json.Unmarshal(actual.Data, &left); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(expected.Data, &right); err != nil {
			t.Fatal(err)
		}
		actual.Data = expected.Data
		if !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(left, right) {
			t.Fatalf("%s round-trip mismatch: %v / %v", encoding, left, right)
		}
	}
}
