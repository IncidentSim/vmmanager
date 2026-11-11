package tgbot

import "testing"

func TestAdminDeliveryReceiptSurvivesRestart(t *testing.T) {
	dir := privateDir(t)
	d, e := OpenDelivery(dir)
	if e != nil {
		t.Fatal(e)
	}
	if d.AdminReported("token") {
		t.Fatal("invented receipt")
	}
	if e = d.MarkAdminReported("token"); e != nil {
		t.Fatal(e)
	}
	d.Close()
	d, e = OpenDelivery(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if !d.AdminReported("token") {
		t.Fatal("lost final delivery receipt")
	}
	if e = d.Advance(50); e != nil {
		t.Fatal(e)
	}
	if !d.AdminReported("token") {
		t.Fatal("offset dropped receipt")
	}
}
