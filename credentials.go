package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const mailKeyEnv = "BESPOKE_MAIL_KEY"

type accountCredential struct {
	AppPassword  string `json:"app_password,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
}

type credentialVault struct {
	aead cipher.AEAD
}

func loadCredentialVault() (*credentialVault, error) {
	encoded := os.Getenv(mailKeyEnv)
	if encoded == "" {
		return nil, fmt.Errorf("%s is not configured", mailKeyEnv)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key", mailKeyEnv)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &credentialVault{aead: aead}, nil
}

func (v *credentialVault) Seal(value accountCredential) ([]byte, []byte, error) {
	if v == nil {
		return nil, nil, errors.New("mail credential encryption is unavailable")
	}
	plaintext, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return v.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func (v *credentialVault) Open(ciphertext, nonce []byte) (accountCredential, error) {
	if v == nil {
		return accountCredential{}, errors.New("mail credential encryption is unavailable")
	}
	plaintext, err := v.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return accountCredential{}, errors.New("decrypt mail credentials")
	}
	var value accountCredential
	if err := json.Unmarshal(plaintext, &value); err != nil {
		return accountCredential{}, errors.New("decode mail credentials")
	}
	return value, nil
}
