package tgbot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryRefusesMissingOrNullHighWatermark(t *testing.T) {
	for _, raw := range []string{`{"version":1,"imported":true}`, `{"version":1,"offset":null,"imported":true}`, `{"version":1,"offset":0}`, `{"version":1,"offset":0,"imported":null}`, `{"version":1,"offset":200,"offset":0,"imported":true}`} {
		t.Run(raw, func(t *testing.T) {
			d := privateDir(t)
			os.WriteFile(filepath.Join(d, "delivery.json"), []byte(raw), 0600)
			if s, e := OpenDelivery(d); e == nil {
				s.Close()
				t.Fatal("accepted ambiguous delivery offset")
			}
		})
	}
}
