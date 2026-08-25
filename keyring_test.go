package main

import "testing"

func TestKeyringAccountScopesTokensToAccountAndGateway(t *testing.T) {
	account, err := keyringAccount("account-a", "gateway-a")
	if err != nil {
		t.Fatal(err)
	}
	if account != "account/account-a/gateway/gateway-a" {
		t.Fatalf("keyring account = %q", account)
	}
	if _, err := keyringAccount("", "gateway-a"); err == nil {
		t.Fatal("missing account ID was accepted")
	}
}

func TestTokenPaddingRoundTrip(t *testing.T) {
	padded, err := padToken("secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := unpadToken([]byte(padded))
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("unpadToken = %q", got)
	}
	if _, err := unpadToken([]byte("too short")); err == nil {
		t.Fatal("short token data was accepted")
	}
}
