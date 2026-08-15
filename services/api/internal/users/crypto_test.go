package users

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	plaintext := "ghs_aevor-test-token-do-not-leak"

	ciphertext, err := Encrypt(plaintext, key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	decrypted, err := Decrypt(ciphertext, key)

	if err != nil {
		t.Fatalf("Decrypt() error: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("round trip = %q, want %q", decrypted, plaintext)
	}
}

func TestEncrypt_ProducesUniqueCiphertexts(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	first, err := Encrypt("same-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	second, err := Encrypt("same-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	if first == second {
		t.Error("Encrypt() produced identical ciphertexts, want a fresh nonce per call")
	}
}

func TestEncrypt_CiphertextIsBase64(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	ciphertext, err := Encrypt("plain", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	if _, err := base64.StdEncoding.DecodeString(ciphertext); err != nil {
		t.Errorf("ciphertext is not base64: %v", err)
	}
}

func TestEncrypt_RejectsInvalidKeyLength(t *testing.T) {
	for _, key := range [][]byte{nil, []byte("short"), bytes.Repeat([]byte("k"), 31), bytes.Repeat([]byte("k"), 33)} {
		if _, err := Encrypt("plain", key); err != ErrInvalidKeyLength {
			t.Errorf("Encrypt() error = %v, want ErrInvalidKeyLength for key length %d", err, len(key))
		}
	}
}

func TestDecrypt_RejectsInvalidKeyLength(t *testing.T) {
	if _, err := Decrypt("ciphertext", []byte("short")); err != ErrInvalidKeyLength {
		t.Errorf("Decrypt() error = %v, want ErrInvalidKeyLength", err)
	}
}

func TestDecrypt_RejectsInvalidBase64(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	if _, err := Decrypt("not-base64!!!", key); err != ErrInvalidCiphertext {
		t.Errorf("Decrypt() error = %v, want ErrInvalidCiphertext for invalid base64", err)
	}
}

func TestDecrypt_RejectsTruncatedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	if _, err := Decrypt(base64.StdEncoding.EncodeToString([]byte("short")), key); err != ErrInvalidCiphertext {
		t.Errorf("Decrypt() error = %v, want ErrInvalidCiphertext for truncated data", err)
	}
}

func TestDecrypt_RejectsTamperedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	ciphertext, err := Encrypt("original-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	data, err := base64.StdEncoding.DecodeString(ciphertext)

	if err != nil {
		t.Fatalf("DecodeString() error: %v", err)
	}

	data[len(data)-1] ^= 0xFF

	tampered := base64.StdEncoding.EncodeToString(data)

	if _, err := Decrypt(tampered, key); err != ErrInvalidCiphertext {
		t.Errorf("Decrypt() error = %v, want ErrInvalidCiphertext for tampered data", err)
	}
}

func TestDecrypt_WrongKeyRejected(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	otherKey := bytes.Repeat([]byte("o"), 32)

	ciphertext, err := Encrypt("secret-token", key)

	if err != nil {
		t.Fatalf("Encrypt() error: %v", err)
	}

	if _, err := Decrypt(ciphertext, otherKey); err != ErrInvalidCiphertext {
		t.Errorf("Decrypt() error = %v, want ErrInvalidCiphertext for a different key", err)
	}
}

func TestDecrypt_EmptyStringRejected(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)

	if _, err := Decrypt("", key); err != ErrInvalidCiphertext {
		t.Errorf("Decrypt() error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestErrorMessagesDoNotIncludePlaintext(t *testing.T) {
	_, err := Encrypt("super-secret-plaintext", []byte("short"))

	if err == nil {
		t.Fatal("Encrypt() succeeded with a short key")
	}

	if strings.Contains(err.Error(), "super-secret-plaintext") {
		t.Errorf("error leaks plaintext: %q", err.Error())
	}
}
