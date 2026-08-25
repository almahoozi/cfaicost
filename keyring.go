package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/99designs/keyring"
)

const (
	keyringServiceName = "cfaicost"
	tokenPaddingBytes  = 16
)

type tokenStore interface {
	Load(accountID, gateway string) (string, error)
	Save(accountID, gateway, token string) error
	Exists(accountID, gateway string) (bool, error)
}

type keyringTokenStore struct{}

func newTokenStore() tokenStore { return keyringTokenStore{} }

func (keyringTokenStore) Load(accountID, gateway string) (string, error) {
	ring, err := openKeyring()
	if err != nil {
		return "", err
	}
	account, err := keyringAccount(accountID, gateway)
	if err != nil {
		return "", err
	}
	item, err := ring.Get(account)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return "", fmt.Errorf("no Cloudflare API token is stored for account %q and gateway %q; run cfaicost set-token", accountID, gateway)
	}
	if err != nil {
		return "", fmt.Errorf("read token from keyring account %q: %w", account, err)
	}
	token, err := unpadToken(item.Data)
	if err != nil {
		return "", fmt.Errorf("read token from keyring account %q: %w", account, err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("empty token in keyring account %q", account)
	}
	return token, nil
}

func (keyringTokenStore) Save(accountID, gateway, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("token is empty")
	}
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	account, err := keyringAccount(accountID, gateway)
	if err != nil {
		return err
	}
	padded, err := padToken(token)
	if err != nil {
		return err
	}
	if err := ring.Set(keyring.Item{Key: account, Data: []byte(padded), Label: "cfaicost://" + account}); err != nil {
		return fmt.Errorf("save token to keyring account %q: %w", account, err)
	}
	return nil
}

func (keyringTokenStore) Exists(accountID, gateway string) (bool, error) {
	ring, err := openKeyring()
	if err != nil {
		return false, err
	}
	account, err := keyringAccount(accountID, gateway)
	if err != nil {
		return false, err
	}
	_, err = ring.Get(account)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read token from keyring account %q: %w", account, err)
	}
	return true, nil
}

func openKeyring() (keyring.Keyring, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName:              keyringServiceName,
		KeychainTrustApplication: true,
		AllowedBackends:          allowedKeyringBackends(),
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring service %q: %w", keyringServiceName, err)
	}
	return ring, nil
}

func allowedKeyringBackends() []keyring.BackendType {
	switch runtime.GOOS {
	case "darwin":
		return []keyring.BackendType{keyring.KeychainBackend, keyring.SecretServiceBackend, keyring.KWalletBackend, keyring.WinCredBackend, keyring.PassBackend}
	case "windows":
		return []keyring.BackendType{keyring.WinCredBackend, keyring.SecretServiceBackend, keyring.KWalletBackend, keyring.PassBackend, keyring.KeychainBackend}
	default:
		return []keyring.BackendType{keyring.SecretServiceBackend, keyring.KWalletBackend, keyring.PassBackend, keyring.KeychainBackend, keyring.WinCredBackend}
	}
}

func keyringAccount(accountID, gateway string) (string, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(gateway) == "" {
		return "", errors.New("account ID and gateway are required for keyring token storage")
	}
	return "account/" + accountID + "/gateway/" + gateway, nil
}

func padToken(token string) (string, error) {
	prefix := make([]byte, tokenPaddingBytes)
	suffix := make([]byte, tokenPaddingBytes)
	if _, err := rand.Read(prefix); err != nil {
		return "", err
	}
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return string(prefix) + token + string(suffix), nil
}

func unpadToken(token []byte) (string, error) {
	if len(token) < tokenPaddingBytes*2 {
		return "", errors.New("stored token is too short")
	}
	return string(token[tokenPaddingBytes : len(token)-tokenPaddingBytes]), nil
}
