package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/pq/mldsa"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
)

const (
	defaultEndpoint = "http://127.0.0.1:8545"
	defaultChainID  = int64(714)
	defaultKeyFile  = "keys/pq_account/privkey.hex"
	defaultGasLimit = uint64(30000)
	statsInterval   = 3 * time.Second
	// pqPubKeySize is the size of an ML-DSA-44 public key in bytes.
	pqPubKeySize = 1312
)

var (
	// pqRegistryAddress is the PQ key registry precompile at 0x70.
	pqRegistryAddress = common.HexToAddress("0x0000000000000000000000000000000000000070")

	// INIT_HOLDER is used as the gas-paying account for funding and registration.
	initHolderKeyHex = "59ba8068eb256d520179e903f43dacf6d8d57d72bd306e1bd603fdb8c8da10e8"

	// minBalance is the minimum BNB balance (in wei) required before benchmarking.
	// Default: 1 BNB — enough for ~47k PQ txs at 10 Gwei gas price, 30k gas each.
	minBalance = new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether))

	endpointFlag  = flag.String("endpoint", defaultEndpoint, "RPC URL")
	chainIDFlag   = flag.Int64("chainId", defaultChainID, "chain ID")
	keyfileFlag   = flag.String("keyfile", defaultKeyFile, "path to privkey.hex file")
	workersFlag   = flag.Int("workers", 1, "number of concurrent senders")
	intervalFlag  = flag.Duration("interval", 200*time.Millisecond, "interval between sends per worker")
	durationFlag  = flag.Duration("duration", 0, "total run time, 0 = run forever")
	toFlag        = flag.String("to", "", "recipient address hex (default: sender sends to self)")
	fundAmountFlag = flag.String("fund", "10", "BNB to transfer from INIT_HOLDER to PQ address if balance is below minimum (0 = skip)")
)

type pqAccount struct {
	PrivKey []byte
	PubKey  []byte
	Address common.Address
}

type runStats struct {
	sent   atomic.Uint64
	errors atomic.Uint64
}

func main() {
	flag.Parse()

	if *workersFlag <= 0 {
		exitf("-workers must be > 0")
	}
	if *intervalFlag <= 0 {
		exitf("-interval must be > 0")
	}

	account, err := loadPQAccount(*keyfileFlag)
	if err != nil {
		exitf("load PQ account: %v", err)
	}

	toAddr, err := resolveRecipient(*toFlag, account.Address)
	if err != nil {
		exitf("resolve recipient: %v", err)
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx := rootCtx
	var cancel context.CancelFunc
	if *durationFlag > 0 {
		ctx, cancel = context.WithTimeout(rootCtx, *durationFlag)
		defer cancel()
	}

	client, err := ethclient.DialContext(ctx, *endpointFlag)
	if err != nil {
		exitf("dial %s: %v", *endpointFlag, err)
	}
	defer client.Close()

	chainID := big.NewInt(*chainIDFlag)

	fmt.Printf("PQ Sender:  %s\n", account.Address.Hex())
	fmt.Printf("Recipient:  %s\n", toAddr.Hex())
	fmt.Printf("Endpoint:   %s\n", *endpointFlag)
	fmt.Printf("Workers:    %d  Interval: %s\n", *workersFlag, *intervalFlag)

	if err := fundIfNeeded(ctx, client, account.Address, chainID); err != nil {
		exitf("funding failed: %v", err)
	}

	if err := ensureRegistered(ctx, client, account, chainID); err != nil {
		exitf("registration failed: %v", err)
	}

	baseNonce, err := client.PendingNonceAt(ctx, account.Address)
	if err != nil {
		exitf("fetch pending nonce: %v", err)
	}

	st := &runStats{}
	var wg sync.WaitGroup
	for i := 0; i < *workersFlag; i++ {
		wg.Add(1)
		go worker(ctx, &wg, client, account, toAddr, chainID,
			uint64(i), uint64(*workersFlag), baseNonce+uint64(i),
			*intervalFlag, st)
	}

	statsDone := make(chan struct{})
	go printStats(ctx, client, account.Address, st, statsDone)

	wg.Wait()
	<-statsDone
	fmt.Printf("Done — sent=%d errors=%d\n", st.sent.Load(), st.errors.Load())
}

// worker sends PQ transactions on a ticker until ctx is cancelled.
func worker(
	ctx context.Context,
	wg *sync.WaitGroup,
	client *ethclient.Client,
	account pqAccount,
	toAddr common.Address,
	chainID *big.Int,
	workerID, stride, startNonce uint64,
	interval time.Duration,
	st *runStats,
) {
	defer wg.Done()

	signer := types.NewPQSigner(chainID)
	gasPrice := big.NewInt(10 * params.GWei)
	nonce := startNonce

	send := func() {
		tx := types.NewTx(&types.PQTxData{
			ChainID:  new(big.Int).Set(chainID),
			Nonce:    nonce,
			GasPrice: new(big.Int).Set(gasPrice),
			Gas:      defaultGasLimit,
			To:       &toAddr,
			Value:    big.NewInt(1),
			From:     account.Address,
		})
		signed, err := types.SignPQTx(tx, signer, account.PrivKey)
		if err != nil {
			st.errors.Add(1)
			fmt.Printf("[worker %d] sign error nonce=%d: %v\n", workerID, nonce, err)
			nonce += stride
			return
		}
		if err := client.SendTransaction(ctx, signed); err != nil {
			st.errors.Add(1)
			fmt.Printf("[worker %d] send error nonce=%d: %v\n", workerID, nonce, err)
			nonce += stride
			return
		}
		st.sent.Add(1)
		nonce += stride
	}

	send()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			send()
		}
	}
}

// printStats prints TPS and pending-tx stats every statsInterval.
func printStats(ctx context.Context, client *ethclient.Client, sender common.Address, st *runStats, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	var lastSent uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			total := st.sent.Load()
			delta := total - lastSent
			lastSent = total
			tps := float64(delta) / statsInterval.Seconds()

			pending := "n/a"
			if p, err := pendingCount(ctx, client, sender); err == nil {
				pending = fmt.Sprintf("%d", p)
			}
			fmt.Printf("[stats] sent=%d errors=%d tps=%.2f pending=%s  %s\n",
				total, st.errors.Load(), tps, pending, time.Now().Format("15:04:05"))
		}
	}
}

func pendingCount(ctx context.Context, client *ethclient.Client, addr common.Address) (uint64, error) {
	pNonce, err := client.PendingNonceAt(ctx, addr)
	if err != nil {
		return 0, err
	}
	cNonce, err := client.NonceAt(ctx, addr, nil)
	if err != nil {
		return 0, err
	}
	if pNonce < cNonce {
		return 0, nil
	}
	return pNonce - cNonce, nil
}

// fundIfNeeded transfers BNB from INIT_HOLDER to the PQ address when the
// current balance is below minBalance. Skipped when -fund=0.
func fundIfNeeded(ctx context.Context, client *ethclient.Client, pqAddr common.Address, chainID *big.Int) error {
	if *fundAmountFlag == "0" {
		return nil
	}

	bal, err := client.BalanceAt(ctx, pqAddr, nil)
	if err != nil {
		return fmt.Errorf("check balance: %w", err)
	}
	if bal.Cmp(minBalance) >= 0 {
		fmt.Printf("PQ address balance: %s wei — no funding needed\n", bal)
		return nil
	}

	// Parse requested amount (integer BNB → wei).
	bnbAmount, ok := new(big.Int).SetString(*fundAmountFlag, 10)
	if !ok || bnbAmount.Sign() <= 0 {
		return fmt.Errorf("invalid -fund value %q (must be a positive integer BNB amount)", *fundAmountFlag)
	}
	weiAmount := new(big.Int).Mul(bnbAmount, big.NewInt(params.Ether))

	fmt.Printf("PQ address balance: %s wei — funding %s BNB from INIT_HOLDER...\n", bal, bnbAmount)

	initKey, err := crypto.HexToECDSA(initHolderKeyHex)
	if err != nil {
		return fmt.Errorf("load init holder key: %w", err)
	}
	initAddr := crypto.PubkeyToAddress(initKey.PublicKey)

	nonce, err := client.PendingNonceAt(ctx, initAddr)
	if err != nil {
		return fmt.Errorf("nonce for init holder: %w", err)
	}

	gasPrice := big.NewInt(10 * params.GWei)
	fundTx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &pqAddr,
		Value:    weiAmount,
		Gas:      21_000,
		GasPrice: gasPrice,
	})
	signer := types.NewEIP155Signer(chainID)
	signed, err := types.SignTx(fundTx, signer, initKey)
	if err != nil {
		return fmt.Errorf("sign funding tx: %w", err)
	}
	if err := client.SendTransaction(ctx, signed); err != nil {
		return fmt.Errorf("send funding tx: %w", err)
	}
	txHash := signed.Hash()
	fmt.Printf("Funding tx sent: %s — waiting for inclusion...\n", txHash.Hex())

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		_, isPending, err := client.TransactionByHash(ctx, txHash)
		if err != nil {
			continue
		}
		if !isPending {
			newBal, _ := client.BalanceAt(ctx, pqAddr, nil)
			fmt.Printf("Funded: %s now has %s wei\n", pqAddr.Hex(), newBal)
			return nil
		}
	}
	return fmt.Errorf("funding tx %s not mined within 60s", txHash.Hex())
}

// ensureRegistered checks whether the PQ address is in the registry; if not,
// it uses INIT_HOLDER (a secp256k1 account) to send a delegate-registration tx
// to the 0x70 precompile. The precompile accepts 20-byte target address +
// 1312-byte pubkey and registers target on behalf of any caller.
func ensureRegistered(ctx context.Context, client *ethclient.Client, account pqAccount, chainID *big.Int) error {
	current, err := lookupRegistry(ctx, client, account.Address)
	if err != nil {
		return fmt.Errorf("registry lookup: %w", err)
	}
	if len(current) == pqPubKeySize && !isAllZero(current) {
		if bytes.Equal(current, account.PubKey) {
			fmt.Printf("Already registered: %s\n", account.Address.Hex())
			return nil
		}
		return fmt.Errorf("address %s registered with a different pubkey", account.Address.Hex())
	}

	fmt.Printf("Registering PQ pubkey for %s via INIT_HOLDER delegate-register...\n", account.Address.Hex())

	// Load INIT_HOLDER secp256k1 key.
	initKey, err := crypto.HexToECDSA(initHolderKeyHex)
	if err != nil {
		return fmt.Errorf("load init holder key: %w", err)
	}
	initAddr := crypto.PubkeyToAddress(initKey.PublicKey)

	// Build delegate-register data: target_addr (20 bytes) || pubkey (1312 bytes).
	data := make([]byte, 0, common.AddressLength+pqPubKeySize)
	data = append(data, account.Address.Bytes()...)
	data = append(data, account.PubKey...)

	nonce, err := client.PendingNonceAt(ctx, initAddr)
	if err != nil {
		return fmt.Errorf("nonce for init holder: %w", err)
	}

	gasPrice := big.NewInt(10 * params.GWei)
	regTx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &pqRegistryAddress,
		Value:    big.NewInt(0),
		Gas:      900_000, // pqRegistryRegisterGas = 820k + buffer
		GasPrice: gasPrice,
		Data:     data,
	})
	signer := types.NewEIP155Signer(chainID)
	signed, err := types.SignTx(regTx, signer, initKey)
	if err != nil {
		return fmt.Errorf("sign registration tx: %w", err)
	}
	if err := client.SendTransaction(ctx, signed); err != nil {
		return fmt.Errorf("send registration tx: %w", err)
	}
	txHash := signed.Hash()
	fmt.Printf("Registration tx sent: %s — waiting for inclusion...\n", txHash.Hex())

	// Poll until mined.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		_, isPending, err := client.TransactionByHash(ctx, txHash)
		if err != nil {
			continue
		}
		if !isPending {
			fmt.Printf("Registration confirmed: %s\n", txHash.Hex())
			return nil
		}
	}
	return errors.New("registration tx not mined within 60s")
}

// lookupRegistry queries the 0x70 precompile for the registered pubkey of addr.
func lookupRegistry(ctx context.Context, client *ethclient.Client, addr common.Address) ([]byte, error) {
	msg := ethereum.CallMsg{To: &pqRegistryAddress, Data: addr.Bytes()}
	return client.CallContract(ctx, msg, nil)
}

func loadPQAccount(path string) (pqAccount, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pqAccount{}, err
	}
	privKey, err := decodeHex(string(raw))
	if err != nil {
		return pqAccount{}, fmt.Errorf("decode private key: %w", err)
	}
	pubKey, err := mldsa.PublicKeyFromPrivate(privKey)
	if err != nil {
		return pqAccount{}, fmt.Errorf("derive public key: %w", err)
	}
	return pqAccount{
		PrivKey: privKey,
		PubKey:  pubKey,
		Address: crypto.PQPubkeyToAddress(pubKey),
	}, nil
}

func resolveRecipient(toVal string, sender common.Address) (common.Address, error) {
	if strings.TrimSpace(toVal) == "" {
		return sender, nil
	}
	if !common.IsHexAddress(toVal) {
		return common.Address{}, fmt.Errorf("invalid -to address %q", toVal)
	}
	return common.HexToAddress(toVal), nil
}

func decodeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return nil, errors.New("empty hex string")
	}
	return hex.DecodeString(s)
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// suppress unused import warning
var _ = ecdsa.PublicKey{}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
