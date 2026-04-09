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

	if err := writeOutputs(*outFlag, addr.Hex(), hex.EncodeToString(pubKey), hex.EncodeToString(privKey)); err != nil {
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

func writeOutputs(outDir, address, pubKeyHex, privKeyHex string) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	files := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "address.txt", data: address + "\n", mode: 0o644},
		{name: "pubkey.hex", data: pubKeyHex + "\n", mode: 0o600},
		{name: "privkey.hex", data: privKeyHex + "\n", mode: 0o600},
	}
	for _, file := range files {
		if err := os.WriteFile(filepath.Join(outDir, file.name), []byte(file.data), file.mode); err != nil {
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
