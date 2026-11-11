package tgbot

import "testing"

func TestAdminIDsCopiedAtConstruction(t *testing.T) {
	b, _, _ := testBot(t)
	priv, _ := writeKeys(t, t.TempDir())
	cfg := b.config
	cfg.AdminUserIDs = []int64{42}
	cfg.JWT = &JWTConfig{PrivateKeyFile: priv, Issuer: jIss, Audience: jAud}
	admin, e := NewBot(cfg, b.tg, b.client, nil)
	if e != nil {
		t.Fatal(e)
	}
	cfg.AdminUserIDs[0] = 43
	if !admin.isAdmin(42) || admin.isAdmin(43) {
		t.Fatal("admin role aliases mutable caller configuration")
	}
}
