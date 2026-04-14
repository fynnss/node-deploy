package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/pq/mldsa"
)

var (
	outFlag  = flag.String("out", "keys/pq_account", "output directory")
	seedFlag = flag.String("seed", "", "optional hex seed for deterministic generation")
)

func main() {
	flag.Parse()

	pubKey, privKey, err := generateKeyPair(*seedFlag)
	if err != nil {
		exitf("generate keypair: %v", err)
	}
	addr := crypto.PQPubkeyToAddress(pubKey)

	if err := writeOutputs(*outFlag, addr.Hex(), pubKey, privKey); err != nil {
		exitf("write outputs: %v", err)
	}

	fmt.Printf("address: %s\n", addr.Hex())
	fmt.Printf("pubkey: %x\n", pubKey)
	fmt.Printf("privkey: %x\n", privKey)
}

func generateKeyPair(seedHex string) ([]byte, []byte, error) {
	if strings.TrimSpace(seedHex) == "" {
		return mldsa.GenerateKey()
	}

	seedBytes, err := decodeHex(seedHex)
	if err != nil {
		return nil, nil, fmt.Errorf("decode -seed: %w", err)
	}
	derivedSeed := sha256.Sum256(seedBytes)
	return mldsa.GenerateKeyFromSeed(derivedSeed[:])
}

func writeOutputs(outDir, address string, pubKey, privKey []byte) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{name: "address.txt", data: []byte(address + "\n"), mode: 0o644},
		{name: "pubkey.hex", data: []byte(hex.EncodeToString(pubKey) + "\n"), mode: 0o600},
		{name: "privkey.hex", data: []byte(hex.EncodeToString(privKey) + "\n"), mode: 0o600},
		// Raw binary forms consumed by geth (--pqvotekey) and by the pq key
		// registry precompile. Emitting these alongside the hex files keeps
		// the tool a single source of truth for both human-readable and
		// machine-readable key material.
		{name: "pubkey.bin", data: pubKey, mode: 0o600},
		{name: "privkey.bin", data: privKey, mode: 0o600},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(outDir, file.name), file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}

func decodeHex(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "0x")
	trimmed = strings.TrimPrefix(trimmed, "0X")
	if trimmed == "" {
		return nil, fmt.Errorf("empty hex string")
	}
	decoded, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
