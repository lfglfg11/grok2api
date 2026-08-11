package model

import "strings"

// IsForced2KImageModel reports whether a public image-model alias always uses
// the 2K Console generation setting. These aliases deliberately keep the
// upstream model unchanged; only the requested resolution is overridden.
func IsForced2KImageModel(publicModel string) bool {
	value := strings.ToLower(strings.TrimSpace(publicModel))
	for _, prefix := range []string{"console/", "web/", "build/"} {
		value = strings.TrimPrefix(value, prefix)
	}
	switch value {
	case "grok-imagine-image-quality-2k", "grok-imagine-image-2k":
		return true
	default:
		return false
	}
}

// ApplyForcedImageResolution returns 2k for a fixed-resolution image alias,
// preserving the supplied value for every other model.
func ApplyForcedImageResolution(publicModel, resolution string) string {
	if IsForced2KImageModel(publicModel) {
		return "2k"
	}
	return resolution
}
