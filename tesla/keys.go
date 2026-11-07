package tesla

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/teslamotors/vehicle-command/pkg/protocol"
)

// LoadOrCreatePrivateKey loads an EC P-256 private key from the given PEM file.
// If the file does not exist, it generates a new key pair and saves it.
//
// The public key is saved alongside (same name with .pub extension) for use
// with the tesla-control add-key-request pairing step.
func LoadOrCreatePrivateKey(keyFile string) (protocol.ECDHPrivateKey, error) {
	if _, err := os.Stat(keyFile); err == nil {
		// Key file exists — load it using the vehicle-command library
		key, err := protocol.LoadPrivateKey(keyFile)
		if err != nil {
			return nil, fmt.Errorf("load private key: %w", err)
		}
		log.Printf("[TESLA] Loaded private key from %s", keyFile)
		return key, nil
	}

	// Generate new key pair
	log.Printf("[TESLA] No private key found at %s — generating new key pair", keyFile)

	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	// Save private key as PEM (SEC1 format, compatible with vehicle-command)
	dir := filepath.Dir(keyFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create key directory: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(ecdsaKey)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	pemBlock := &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(pemBlock), 0600); err != nil {
		return nil, fmt.Errorf("save private key: %w", err)
	}

	// Save public key as PEM for the pairing step
	pubFile := keyFile + ".pub"
	pubBytes, err := x509.MarshalPKIXPublicKey(&ecdsaKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}
	if err := os.WriteFile(pubFile, pem.EncodeToMemory(pubBlock), 0644); err != nil {
		return nil, fmt.Errorf("save public key: %w", err)
	}

	log.Printf("[TESLA] Generated new key pair: %s (private), %s (public)", keyFile, pubFile)
	log.Printf("[TESLA] *** To pair with the car: ***")
	log.Printf("[TESLA]   1. Run: tesla-control -ble add-key-request %s owner cloud_key", pubFile)
	log.Printf("[TESLA]   2. Tap your NFC card on the center console")
	log.Printf("[TESLA]   3. Confirm on the car's touchscreen")

	// Reload through the library so we get the correct internal key type
	return protocol.LoadPrivateKey(keyFile)
}
