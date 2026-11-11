package nextermaccess

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoginStockNumericTOTPFlag(t *testing.T) {
	for _, tc := range []struct {
		flag    string
		allowed bool
	}{
		{`0`, true}, {`false`, true}, {`1`, false}, {`true`, false},
		{`2`, false}, {`null`, false}, {`"0"`, false}, {`{}`, false},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"token":"fixture-session","totpRequired":%s}`, tc.flag)
			}))
			defer s.Close()
			c, err := NewClient(s.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			token, err := c.Login(context.Background(), "fixture", Secret("fixture"))
			if tc.allowed && (err != nil || token != "fixture-session") {
				t.Fatal("valid stock login rejected", err)
			}
			if !tc.allowed && err == nil {
				t.Fatal("unproven password-only login accepted")
			}
		})
	}
}
