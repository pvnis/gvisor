// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package amdconf contains AMD GPU configuration that runsc needs without
// depending on the Sentry.
package amdconf

import (
	"fmt"
	"math/bits"
	"strings"
)

// CUMask is a set of the GPU's compute units, one bit per unit, in the layout
// AMDKFD_IOC_SET_CU_MASK expects: word i holds units 32i through 32i+31, least
// significant bit first.
//
// A queue only runs on the units its mask selects, and the mask is enforced by
// the GPU's command processor rather than by anything the sandbox can reach.
// That makes it a partition of the device rather than a limit on how often a
// sandbox may use it: unlike a time-based throttle, it needs no cooperation
// from the workload and no periodic intervention from the Sentry.
//
// A nil CUMask means the sandbox may use every compute unit.
type CUMask []uint32

// Empty returns true if m selects no compute units.
func (m CUMask) Empty() bool {
	for _, w := range m {
		if w != 0 {
			return false
		}
	}
	return true
}

// Bits returns the number of compute units m can describe, which is what
// KFDIoctlSetCUMaskArgs.NumCUMask counts.
func (m CUMask) Bits() uint32 {
	return uint32(len(m)) * 32
}

// Count returns the number of compute units m selects.
func (m CUMask) Count() int {
	n := 0
	for _, w := range m {
		n += bits.OnesCount32(w)
	}
	return n
}

// String implements fmt.Stringer.String, printing the mask as the hexadecimal
// integer it represents, most significant word first.
func (m CUMask) String() string {
	if len(m) == 0 {
		return "0x0"
	}
	// Skip leading zero words so that a mask reads as the integer it is,
	// rather than being padded out to the length it happens to be stored in.
	hi := len(m) - 1
	for hi > 0 && m[hi] == 0 {
		hi--
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "0x%x", m[hi])
	for i := hi - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, "%08x", m[i])
	}
	return sb.String()
}

// Clamp restricts req to the units m selects, in place. Words beyond m's
// length are cleared: a mask this package does not describe cannot be assumed
// to fall inside the ceiling.
//
// The application is free to use fewer units than it has been given, so
// narrowing its own mask is allowed; only widening past the ceiling is not.
func (m CUMask) Clamp(req []uint32) {
	for i := range req {
		if i < len(m) {
			req[i] &= m[i]
		} else {
			req[i] = 0
		}
	}
}

// IsSubsetOf returns whether every compute unit m selects is also selected by
// other. This is what makes it safe to let a container narrow its own mask
// through an annotation: the result can restrict it further but never grant a
// unit the runtime did not.
func (m CUMask) IsSubsetOf(other CUMask) bool {
	for i, w := range m {
		var o uint32
		if i < len(other) {
			o = other[i]
		}
		if w&^o != 0 {
			return false
		}
	}
	return true
}

// ParseCUMask parses a compute unit mask written as a hexadecimal integer,
// with or without a leading "0x". The least significant bit is compute unit
// 0. An empty string parses to a nil mask, meaning every compute unit.
func ParseCUMask(s string) (CUMask, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if digits == "" {
		return nil, fmt.Errorf("%q has no hexadecimal digits", s)
	}
	if len(digits) > maxCUMaskHexDigits {
		return nil, fmt.Errorf("%q describes more than %d compute units", s, maxCUMaskHexDigits*4)
	}
	// Parse from the least significant end, eight hexadecimal digits at a
	// time, so that each group is exactly one word of the mask.
	var mask CUMask
	for end := len(digits); end > 0; end -= 8 {
		start := end - 8
		if start < 0 {
			start = 0
		}
		var w uint32
		if _, err := fmt.Sscanf(digits[start:end], "%x", &w); err != nil {
			return nil, fmt.Errorf("%q is not a hexadecimal integer", s)
		}
		mask = append(mask, w)
	}
	// The driver requires the mask to be a whole number of words, which it
	// already is, but trailing zero words carry no information.
	for len(mask) > 1 && mask[len(mask)-1] == 0 {
		mask = mask[:len(mask)-1]
	}
	return mask, nil
}

// maxCUMaskHexDigits bounds a parsed mask at 1024 compute units, far above
// what any AMD GPU has, so that a mistaken value is rejected rather than
// turned into a large allocation.
const maxCUMaskHexDigits = 256
