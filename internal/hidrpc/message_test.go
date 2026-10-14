package hidrpc

import (
	"encoding/binary"
	"testing"
)

func message(t MessageType, d ...byte) Message {
	return Message{t: t, d: d}
}

// Any session holder controls these bytes; a short or inconsistent message
// must be an error, never a panic (a panic in a HID queue ends the app, D15).
func TestReportsRejectShortMessages(t *testing.T) {
	parsers := []struct {
		name  string
		t     MessageType
		min   int
		parse func(Message) error
	}{
		{"keypress", TypeKeypressReport, 2, func(m Message) error { _, err := m.KeypressReport(); return err }},
		{"keyboard", TypeKeyboardReport, 1, func(m Message) error { _, err := m.KeyboardReport(); return err }},
		{"macro", TypeKeyboardMacroReport, 5, func(m Message) error { _, err := m.KeyboardMacroReport(); return err }},
		{"pointer", TypePointerReport, 9, func(m Message) error { _, err := m.PointerReport(); return err }},
		{"mouse", TypeMouseReport, 3, func(m Message) error { _, err := m.MouseReport(); return err }},
		{"wheel", TypeWheelReport, 1, func(m Message) error { _, err := m.WheelReport(); return err }},
		{"macro state", TypeKeyboardMacroState, 2, func(m Message) error { _, err := m.KeyboardMacroState(); return err }},
	}
	for _, p := range parsers {
		t.Run(p.name, func(t *testing.T) {
			for n := 0; n < p.min; n++ {
				if err := p.parse(message(p.t, make([]byte, n)...)); err == nil {
					t.Fatalf("%d bytes: no error", n)
				}
			}
		})
	}
}

func TestKeyboardMacroReportLength(t *testing.T) {
	macro := func(steps uint32, extra int) *Message {
		d := make([]byte, 5+extra)
		binary.BigEndian.PutUint32(d[1:5], steps)
		m := message(TypeKeyboardMacroReport, d...)
		return &m
	}

	r, err := macro(2, 18).KeyboardMacroReport()
	if err != nil || len(r.Steps) != 2 {
		t.Fatalf("two steps: %d steps, %v", len(r.Steps), err)
	}
	// 954437177*9+5 is 6 modulo 2^32: with 32-bit int arithmetic (the
	// device's) a one-byte body passed the length check, then the parser
	// allocated and indexed for 954437177 steps.
	if _, err := macro(954437177, 1).KeyboardMacroReport(); err == nil {
		t.Fatal("wrapping step count accepted")
	}
	if _, err := macro(3, 18).KeyboardMacroReport(); err == nil {
		t.Fatal("step count beyond the body accepted")
	}
}
