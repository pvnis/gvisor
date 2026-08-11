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

package amdconf

import (
	"slices"
	"testing"
)

func TestParseCUMask(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    CUMask
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "0xff", want: CUMask{0xff}},
		{in: "ff", want: CUMask{0xff}},
		{in: "0xFFFFFFFF", want: CUMask{0xffffffff}},
		// More than one word: the least significant word comes first.
		{in: "0x1ffffffff", want: CUMask{0xffffffff, 0x1}},
		{in: "0xdeadbeefcafe", want: CUMask{0xbeefcafe, 0xdead}},
		// A mask covering 54 compute units, as this GPU has.
		{in: "0x3fffffffffffff", want: CUMask{0xffffffff, 0x3fffff}},
		{in: "0x0", want: CUMask{0}},
		{in: "0x", wantErr: true},
		{in: "nothex", wantErr: true},
	} {
		got, err := ParseCUMask(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCUMask(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCUMask(%q) failed: %v", tc.in, err)
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("ParseCUMask(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCUMaskClamp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling CUMask
		req     []uint32
		want    []uint32
	}{
		{
			name:    "asking for everything yields only what was assigned",
			ceiling: CUMask{0xff},
			req:     []uint32{0xffffffff, 0xffffffff},
			want:    []uint32{0xff, 0},
		},
		{
			name:    "asking for a subset is left alone",
			ceiling: CUMask{0xff},
			req:     []uint32{0x0f},
			want:    []uint32{0x0f},
		},
		{
			name:    "asking for units outside the ceiling yields none",
			ceiling: CUMask{0xff},
			req:     []uint32{0xff00},
			want:    []uint32{0},
		},
		{
			name:    "words beyond the ceiling's length are cleared",
			ceiling: CUMask{0xffffffff},
			req:     []uint32{0xffffffff, 0xffffffff, 0xffffffff},
			want:    []uint32{0xffffffff, 0, 0},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Clone(tc.req)
			tc.ceiling.Clamp(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Clamp(%#x) = %#x, want %#x", tc.req, got, tc.want)
			}
		})
	}
}

func TestCUMaskIsSubsetOf(t *testing.T) {
	for _, tc := range []struct {
		m, other CUMask
		want     bool
	}{
		{m: CUMask{0x0f}, other: CUMask{0xff}, want: true},
		{m: CUMask{0xff}, other: CUMask{0xff}, want: true},
		{m: CUMask{0xff}, other: CUMask{0x0f}, want: false},
		{m: CUMask{0xff00}, other: CUMask{0x00ff}, want: false},
		// A word the ceiling does not have at all is not granted.
		{m: CUMask{0, 0x1}, other: CUMask{0xffffffff}, want: false},
		{m: CUMask{0x1, 0}, other: CUMask{0xffffffff}, want: true},
		// Anything is a subset of an unlimited (nil) mask only if empty;
		// callers treat nil as "no ceiling" before calling this.
		{m: CUMask{0x1}, other: nil, want: false},
	} {
		if got := tc.m.IsSubsetOf(tc.other); got != tc.want {
			t.Errorf("%v.IsSubsetOf(%v) = %v, want %v", tc.m, tc.other, got, tc.want)
		}
	}
}

func TestCUMaskString(t *testing.T) {
	for _, tc := range []struct {
		m    CUMask
		want string
	}{
		{m: CUMask{0xff}, want: "0xff"},
		{m: CUMask{0xff, 0}, want: "0xff"},
		{m: CUMask{0xffffffff, 0x1}, want: "0x1ffffffff"},
		{m: nil, want: "0x0"},
	} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("CUMask(%#x).String() = %q, want %q", []uint32(tc.m), got, tc.want)
		}
	}
}

// The cases here are the masks measured against a Navi 32 (gfx1101): every
// mask whose groups are whole was accepted by the driver and ran, and every
// mask that split a pair was rejected with EINVAL and hung the container.
func TestCUMaskFirstSplitGroup(t *testing.T) {
	for _, tc := range []struct {
		m    CUMask
		n    int
		want int
	}{
		// Whole pairs: no split.
		{m: CUMask{0x3}, n: 2, want: -1},
		{m: CUMask{0xc}, n: 2, want: -1},
		{m: CUMask{0xf}, n: 2, want: -1},
		{m: CUMask{0x33}, n: 2, want: -1},
		{m: CUMask{0x3ffffff}, n: 2, want: -1},
		// Half pairs: the lowest selected unit whose partner is missing.
		{m: CUMask{0x1}, n: 2, want: 0},
		{m: CUMask{0x2}, n: 2, want: 1},
		{m: CUMask{0x5}, n: 2, want: 0},
		{m: CUMask{0x7}, n: 2, want: 2},
		{m: CUMask{0xaa}, n: 2, want: 1},
		{m: CUMask{0x1fff}, n: 2, want: 12},
		{m: CUMask{0x7ffffff}, n: 2, want: 26},
		// A split across a word boundary is still a split.
		{m: CUMask{0x0, 0x1}, n: 2, want: 32},
		{m: CUMask{0x0, 0x3}, n: 2, want: -1},
		// Groups of one, or of none, constrain nothing.
		{m: CUMask{0x5}, n: 1, want: -1},
		{m: CUMask{0x5}, n: 0, want: -1},
		// An empty mask selects nothing, so it splits nothing.
		{m: CUMask{0x0}, n: 2, want: -1},
	} {
		if got := tc.m.FirstSplitGroup(tc.n); got != tc.want {
			t.Errorf("CUMask(%#x).FirstSplitGroup(%d) = %d, want %d", []uint32(tc.m), tc.n, got, tc.want)
		}
		if got, want := tc.m.AlignedTo(tc.n), tc.want < 0; got != want {
			t.Errorf("CUMask(%#x).AlignedTo(%d) = %v, want %v", []uint32(tc.m), tc.n, got, want)
		}
	}
}

func TestCUMaskAlignedDownTo(t *testing.T) {
	for _, tc := range []struct {
		m    CUMask
		n    int
		want CUMask
	}{
		{m: CUMask{0x7}, n: 2, want: CUMask{0x3}},
		{m: CUMask{0x5}, n: 2, want: CUMask{0x0}},
		{m: CUMask{0xaa}, n: 2, want: CUMask{0x0}},
		{m: CUMask{0x1fff}, n: 2, want: CUMask{0xfff}},
		{m: CUMask{0x7ffffff}, n: 2, want: CUMask{0x3ffffff}},
		// Already aligned masks are returned unchanged.
		{m: CUMask{0xf}, n: 2, want: CUMask{0xf}},
		{m: CUMask{0x33}, n: 2, want: CUMask{0x33}},
		// Groups of one never clear anything.
		{m: CUMask{0x5}, n: 1, want: CUMask{0x5}},
	} {
		got := tc.m.AlignedDownTo(tc.n)
		if len(got) != len(tc.want) {
			t.Errorf("CUMask(%#x).AlignedDownTo(%d) = %#x, want %#x", []uint32(tc.m), tc.n, []uint32(got), []uint32(tc.want))
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("CUMask(%#x).AlignedDownTo(%d) = %#x, want %#x", []uint32(tc.m), tc.n, []uint32(got), []uint32(tc.want))
				break
			}
		}
		// Aligning down must never widen the share: the result is always a
		// subset of the original.
		if !got.IsSubsetOf(tc.m) {
			t.Errorf("CUMask(%#x).AlignedDownTo(%d) = %#x, which is not a subset", []uint32(tc.m), tc.n, []uint32(got))
		}
		if !got.AlignedTo(tc.n) {
			t.Errorf("CUMask(%#x).AlignedDownTo(%d) = %#x, which is still not aligned", []uint32(tc.m), tc.n, []uint32(got))
		}
	}
}
