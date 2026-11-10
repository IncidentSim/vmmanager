package tgbot

import (
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

func TestImageLinePresentation(t *testing.T) {
	cases := []struct {
		im             controlclient.Image
		wantDelete     bool
		wantDeleting   bool
		wantCommonWord bool
	}{
		{controlclient.Image{ID: "base", Name: "base", Shared: true, Phase: "ready"}, false, false, true},
		{controlclient.Image{ID: "img-a", Name: "a", Shared: true, Phase: "ready", CanDelete: true}, true, false, true},
		{controlclient.Image{ID: "img-b", Name: "b", Shared: true, Phase: "deleting", CanDelete: true}, true, true, true},
	}
	for _, c := range cases {
		line := imageLine(c.im)
		if strings.Contains(line, "/deleteimage "+c.im.ID) != c.wantDelete {
			t.Fatalf("delete hint %q", line)
		}
		if strings.Contains(line, "удаляется") != c.wantDeleting {
			t.Fatalf("deleting marker %q", line)
		}
		if strings.Contains(line, "общий") != c.wantCommonWord || strings.Contains(line, "личный") {
			t.Fatalf("common label %q", line)
		}
	}
}
