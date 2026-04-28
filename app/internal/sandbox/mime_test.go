package sandbox

import "testing"

func TestMimeFromPath(t *testing.T) {
	cases := map[string]string{
		"chart.png":          "image/png",
		"photo.JPG":          "image/jpeg",
		"data.csv":           "text/csv",
		"report.md":          "text/markdown",
		"script.py":          "text/x-python",
		"weird.unknown":      "application/octet-stream",
		"no-extension":       "application/octet-stream",
		"sub/dir/file.json":  "application/json",
		"DATA.YAML":          "application/x-yaml",
	}
	for in, want := range cases {
		if got := MimeFromPath(in); got != want {
			t.Errorf("MimeFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObjectTypeForMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":              "image",
		"image/jpeg":             "image",
		"image/svg+xml":          "image",
		"text/markdown":          "report",
		"text/csv":               "blob",
		"application/json":       "blob",
		"application/octet-stream": "blob",
		"":                       "blob",
	}
	for in, want := range cases {
		if got := ObjectTypeForMIME(in); got != want {
			t.Errorf("ObjectTypeForMIME(%q) = %q, want %q", in, got, want)
		}
	}
}
