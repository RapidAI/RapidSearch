package browser

import (
	"fmt"
	"testing"
)

func TestNewInstanceStableViewport(t *testing.T) {
	cases := []struct {
		id, w, h int
	}{
		{0, 1280, 800},
		{1, 1280, 800},
		{2, 1280, 800},
		{3, 1280, 800},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("id%d", tc.id), func(t *testing.T) {
			for i := 0; i < 8; i++ {
				in := newInstance(tc.id, Options{Display: ":3"})
				if in.width != tc.w || in.height != tc.h {
					t.Fatalf("try %d: got %dx%d want %dx%d", i, in.width, in.height, tc.w, tc.h)
				}
			}
		})
	}
}
