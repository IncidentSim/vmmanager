package vmbot

import "testing"

func TestConfiguredSystemImageOverridesCreatorProjection(t *testing.T) {
	r, id := commonRig(t)
	r.c.control.SharedImages = append(r.c.control.SharedImages, id)
	count := 0
	for _, i := range r.c.ControlImages(11) {
		if i.ID == id {
			count++
			if i.CanDelete {
				t.Fatal("configured system image incorrectly offers creator deletion")
			}
		}
	}
	if count != 1 {
		t.Fatalf("catalog contains %d entries for a protected system image, want 1", count)
	}
	stored := r.st.Snapshot().Images
	if len(stored) != 1 || stored[0].ID != id || stored[0].OwnerUserID != 11 {
		t.Fatal("protecting image changed its stable creator record")
	}
}
