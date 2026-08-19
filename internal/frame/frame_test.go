package frame

import (
	"bytes"
	"errors"
	"testing"
)

func TestRoundTripSeveralFrames(t *testing.T) {
	want := [][]byte{
		[]byte("first packet"),
		{},
		bytes.Repeat([]byte{0xAB}, 1400),
	}
	var buf []byte
	for _, p := range want {
		var err error
		if buf, err = Append(buf, TypeIP, p); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	var got [][]byte
	err := Walk(buf, func(typ byte, payload []byte) error {
		if typ != TypeIP {
			t.Errorf("type = %#x, want %#x", typ, TypeIP)
		}
		got = append(got, bytes.Clone(payload))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d differs: got %d bytes, want %d", i, len(got[i]), len(want[i]))
		}
	}
}

func TestPaddingIsSkippedAndSizedExactly(t *testing.T) {
	buf, err := Append(nil, TypeIP, []byte("real"))
	if err != nil {
		t.Fatal(err)
	}
	before := len(buf)
	if buf, err = AppendPadding(buf, 64); err != nil {
		t.Fatalf("pad: %v", err)
	}
	if got := len(buf) - before; got != 64 {
		t.Errorf("padding added %d bytes, want exactly 64", got)
	}

	var seen int
	if err := Walk(buf, func(typ byte, payload []byte) error {
		seen++
		if typ == TypePad {
			t.Error("padding frames must not be handed to the caller")
		}
		if string(payload) != "real" {
			t.Errorf("payload = %q, want \"real\"", payload)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if seen != 1 {
		t.Errorf("walked %d frames, want 1 real frame", seen)
	}
}

func TestPaddingSmallerThanItsHeaderIsRejected(t *testing.T) {
	if _, err := AppendPadding(nil, HeaderSize-1); err == nil {
		t.Error("padding smaller than the frame header cannot be represented")
	}
}

func TestTruncatedDatagramIsReported(t *testing.T) {
	buf, _ := Append(nil, TypeIP, []byte("hello"))
	for _, cut := range []int{1, 2, HeaderSize, len(buf) - 1} {
		if err := Walk(buf[:cut], func(byte, []byte) error { return nil }); !errors.Is(err, ErrTruncated) {
			t.Errorf("cut to %d bytes: err = %v, want ErrTruncated", cut, err)
		}
	}
}

func TestWalkStopsOnCallbackError(t *testing.T) {
	buf, _ := Append(nil, TypeIP, []byte("a"))
	buf, _ = Append(buf, TypeIP, []byte("b"))
	boom := errors.New("boom")
	n := 0
	err := Walk(buf, func(byte, []byte) error { n++; return boom })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the callback's error", err)
	}
	if n != 1 {
		t.Errorf("callback ran %d times, want 1 — Walk should stop at the first error", n)
	}
}

func BenchmarkAppendAndWalk(b *testing.B) {
	payload := bytes.Repeat([]byte{0x5A}, 1300)
	buf := make([]byte, 0, 1500)
	b.ReportAllocs()
	for b.Loop() {
		buf = buf[:0]
		buf, _ = Append(buf, TypeIP, payload)
		_ = Walk(buf, func(byte, []byte) error { return nil })
	}
}
