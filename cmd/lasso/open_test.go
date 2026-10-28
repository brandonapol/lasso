package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/brandonapol/lasso/internal/api"
)

func TestSignInCode(t *testing.T) {
	srv := httptest.NewServer(api.Handler(api.NewFakeSource(), api.Options{Token: "tok", Port: 7337}))
	defer srv.Close()
	code, err := signInCode(context.Background(), srv.URL, "tok")
	if err != nil || code == "" {
		t.Fatalf("code %q, err %v", code, err)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(srv.URL + "/login?code=" + code)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 1 {
		t.Fatalf("login: %d %v", resp.StatusCode, resp.Cookies())
	}
	if _, err := signInCode(context.Background(), srv.URL, "wrong"); err == nil {
		t.Fatal("a wrong token got a code")
	}
}
