package protocol

import (
	"strings"
	"testing"
)

func TestChecksAttachments(t *testing.T) {
	png := "data:image/png;base64,iVBORw0KGgo="
	for _, c := range []struct {
		name string
		a    Attachments
		want string
	}{
		{"fine", Attachments{Images: []string{png}, Files: []File{{Name: "a.txt", Text: "hi"}}}, ""},
		{"too many images", Attachments{Images: make([]string, MaxImages+1)}, "at most 10 images"},
		{"not an image", Attachments{Images: []string{"data:image/svg+xml;base64,PHN2Zz4="}}, "PNG, JPEG"},
		{"not base64", Attachments{Images: []string{"data:image/png;base64,%%%"}}, "PNG, JPEG"},
		{"empty image", Attachments{Images: []string{"data:image/png;base64,"}}, "PNG, JPEG"},
		{"no name", Attachments{Files: []File{{Text: "hi"}}}, "needs a name"},
		{"too large", Attachments{Files: []File{{Name: "big.txt", Text: strings.Repeat("x", MaxFileBytes+1)}}}, "larger than 512 KB"},
	} {
		if got := c.a.Check(); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: Check() = %q, want %q", c.name, got, c.want)
		}
	}
}
