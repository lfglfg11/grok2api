package model

import "testing"

func TestApplyForcedImageResolution(t *testing.T) {
	for _, test := range []struct {
		model      string
		resolution string
		want       string
	}{
		{model: "grok-imagine-image-2k", resolution: "", want: "2k"},
		{model: "Console/grok-imagine-image-quality-2k", resolution: "1k", want: "2k"},
		{model: "Console/grok-imagine-image-2.0-2k", resolution: "1k", want: "2k"},
		{model: "grok-imagine-image", resolution: "1k", want: "1k"},
	} {
		if got := ApplyForcedImageResolution(test.model, test.resolution); got != test.want {
			t.Errorf("ApplyForcedImageResolution(%q, %q) = %q, want %q", test.model, test.resolution, got, test.want)
		}
	}
}
