package main

import (
	"encoding/json"
	"fmt"
	"math"
)

// pxNum is a pixel length arriving from a pointer gesture.
//
// It exists because `int` was wrong and wrong invisibly. A pointer coordinate is
// fractional on any display that is not at 1:1 device pixels, so a drag commits
// a height of 130.51953125 — and `json.Unmarshal` will not put that in an `int`.
// The command answered 400, the browser dropped it, and the row simply did not
// move: no error anywhere the user could see and nothing in the server's log,
// because the request failed before it was ever logged as a command.
//
// A client sending more precision than the server needs is not a malformed
// request. The pixel the user pointed at is the answer either way, so round it
// and carry on. Refusing has no upside — there is no correct behaviour being
// protected, only a wire representation being enforced.
type pxNum int

func (p *pxNum) UnmarshalJSON(b []byte) error {
	// `null` is not a length. json.Unmarshal treats it as "leave the value
	// alone", which would silently pass whatever the zero value happens to be
	// down to a clamp that has no way to tell it apart from a real measurement.
	if string(b) == "null" {
		return fmt.Errorf("pixel length is null")
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("pixel length: %w", err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("pixel length is not a number")
	}
	// Clamped before the conversion: a float outside int range is undefined
	// behaviour on conversion, and the callers clamp to their own bounds anyway.
	f = math.Round(f)
	if f > math.MaxInt32 {
		f = math.MaxInt32
	}
	if f < math.MinInt32 {
		f = math.MinInt32
	}
	*p = pxNum(f)
	return nil
}

func (p pxNum) Int() int { return int(p) }
