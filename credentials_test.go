package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestCredentialVaultRoundTrip(t *testing.T) {
	t.Setenv(mailKeyEnv, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	vault, err := loadCredentialVault()
	if err != nil {
		t.Fatal(err)
	}
	want := accountCredential{AppPassword: "not-a-real-password", RefreshToken: "refresh"}
	ciphertext, nonce, err := vault.Seal(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), want.AppPassword) {
		t.Fatal("ciphertext contains plaintext password")
	}
	got, err := vault.Open(ciphertext, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Open() = %#v, want %#v", got, want)
	}
}

func TestCredentialVaultRejectsInvalidKey(t *testing.T) {
	t.Setenv(mailKeyEnv, base64.StdEncoding.EncodeToString([]byte("too short")))
	if _, err := loadCredentialVault(); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestCredentialVaultRejectsTampering(t *testing.T) {
	t.Setenv(mailKeyEnv, base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	vault, err := loadCredentialVault()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := vault.Seal(accountCredential{AccessToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[0] ^= 0xff
	if _, err := vault.Open(ciphertext, nonce); err == nil {
		t.Fatal("expected authentication failure for modified ciphertext")
	}
}
