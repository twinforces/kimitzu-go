package wallet

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"

	"github.com/OpenBazaar/multiwallet/config"
	wi "github.com/OpenBazaar/wallet-interface"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v2"

	"github.com/OpenBazaar/go-ethwallet/util"
)

const (
	// InfuraAPIKey is the hard coded Infura API key
	InfuraAPIKey = "openbazaar"
)

// EthConfiguration - used for eth specific configuration
type EthConfiguration struct {
	RopstenPPAddress string `yaml:"ROPSTEN_PPv2_ADDRESS"`
	RegistryAddress  string `yaml:"ROPSTEN_REGISTRY"`
}

// EthRedeemScript - used to represent redeem script for eth wallet
// <uniqueId: 20><threshold:1><timeoutHours:4><buyer:20><seller:20>
// <moderator:20><multisigAddress:20><tokenAddress:20>
type EthRedeemScript struct {
	TxnID           common.Address
	Threshold       uint8
	Timeout         uint32
	Buyer           common.Address
	Seller          common.Address
	Moderator       common.Address
	MultisigAddress common.Address
	TokenAddress    common.Address
}

// SerializeEthScript - used to serialize eth redeem script
func SerializeEthScript(scrpt EthRedeemScript) ([]byte, error) {
	b := bytes.Buffer{}
	e := gob.NewEncoder(&b)
	err := e.Encode(scrpt)
	return b.Bytes(), err
}

// DeserializeEthScript - used to deserialize eth redeem script
func DeserializeEthScript(b []byte) (EthRedeemScript, error) {
	scrpt := EthRedeemScript{}
	buf := bytes.NewBuffer(b)
	d := gob.NewDecoder(buf)
	err := d.Decode(&scrpt)
	return scrpt, err
}

// PendingTxn used to record a pending eth txn
type PendingTxn struct {
	TxnID   common.Hash
	OrderID string
	Amount  int64
	Nonce   int32
	From    string
	To      string
}

// SerializePendingTxn - used to serialize eth pending txn
func SerializePendingTxn(pTxn PendingTxn) ([]byte, error) {
	b := bytes.Buffer{}
	e := gob.NewEncoder(&b)
	err := e.Encode(pTxn)
	return b.Bytes(), err
}

// DeserializePendingTxn - used to deserialize eth pending txn
func DeserializePendingTxn(b []byte) (PendingTxn, error) {
	pTxn := PendingTxn{}
	buf := bytes.NewBuffer(b)
	d := gob.NewDecoder(buf)
	err := d.Decode(&pTxn)
	return pTxn, err
}

// GenScriptHash - used to generate script hash for eth as per
// escrow smart contract
func GenScriptHash(script EthRedeemScript) ([32]byte, string, error) {
	ahash := sha3.NewKeccak256()
	a := make([]byte, 4)
	binary.BigEndian.PutUint32(a, script.Timeout)
	arr := append(script.TxnID.Bytes(), append([]byte{script.Threshold},
		append(a[:], append(script.Buyer.Bytes(),
			append(script.Seller.Bytes(), append(script.Moderator.Bytes(),
				append(script.MultisigAddress.Bytes())...)...)...)...)...)...)
	ahash.Write(arr)
	var retHash [32]byte

	copy(retHash[:], ahash.Sum(nil)[:])
	ahashStr := hexutil.Encode(retHash[:])

	return retHash, ahashStr, nil
}

// EthereumWallet is the wallet implementation for ethereum
type EthereumWallet struct {
	client        *EthClient
	account       *Account
	address       *EthAddress
	service       *Service
	registry      *Registry
	ppsct         *Escrow
	db            wi.Datastore
	exchangeRates wi.ExchangeRates
	listeners     []func(wi.TransactionCallback)
}

// NewEthereumWalletWithKeyfile will return a reference to the Eth Wallet
func NewEthereumWalletWithKeyfile(url, keyFile, passwd string) *EthereumWallet {
	client, err := NewEthClient(url)
	if err != nil {
		log.Fatalf("error initializing wallet: %v", err)
	}
	var myAccount *Account
	myAccount, err = NewAccountFromKeyfile(keyFile, passwd)
	if err != nil {
		log.Fatalf("key file validation failed: %s", err.Error())
	}
	addr := myAccount.Address()

	_, filename, _, _ := runtime.Caller(0)
	conf, err := ioutil.ReadFile(path.Join(path.Dir(filename), "../configuration.yaml"))

	if err != nil {
		log.Fatalf("ethereum config not found: %s", err.Error())
	}
	ethConfig := EthConfiguration{}
	err = yaml.Unmarshal(conf, &ethConfig)
	if err != nil {
		log.Fatalf("ethereum config not valid: %s", err.Error())
	}

	reg, err := NewRegistry(common.HexToAddress(ethConfig.RegistryAddress), client)
	if err != nil {
		log.Fatalf("error initilaizing contract failed: %s", err.Error())
	}

	//reg.GetVersionDetails()

	//smtct, err := NewWallet(common.HexToAddress(ethConfig.RopstenPPAddress), client)
	//if err != nil {
	//	log.Fatalf("error initilaizing contract failed: %s", err.Error())
	//}

	return &EthereumWallet{client, myAccount, &EthAddress{&addr}, &Service{}, reg, nil, nil, nil, []func(wi.TransactionCallback){}}
}

// NewEthereumWallet will return a reference to the Eth Wallet
func NewEthereumWallet(cfg config.CoinConfig, params *chaincfg.Params, mnemonic string, proxy proxy.Dialer) (*EthereumWallet, error) {
	client, err := NewEthClient(cfg.ClientAPIs[0] + "/" + InfuraAPIKey)
	if err != nil {
		log.Errorf("error initializing wallet: %v", err)
		return nil, err
	}
	var myAccount *Account
	/*
		seed := bip39.NewSeed(mnemonic, "")

		privateKeyECDSA, err := crypto.ToECDSA(seed)
		if err != nil {
			return nil
		}

		key := &keystore.Key{
			Address:    crypto.PubkeyToAddress(privateKeyECDSA.PublicKey),
			PrivateKey: privateKeyECDSA,
		}

		myAccount = &Account{key}
	*/
	myAccount, err = NewAccountFromMnemonic(mnemonic, "", params)
	if err != nil {
		log.Errorf("mnemonic based pk generation failed: %s", err.Error())
		return nil, err
	}
	addr := myAccount.Address()

	ethConfig := EthConfiguration{}

	var regAddr interface{}
	var ok bool
	if regAddr, ok = cfg.Options["RegistryAddress"]; !ok {
		log.Errorf("ethereum registry not found: %s", err.Error())
		return nil, err
	}

	ethConfig.RegistryAddress = regAddr.(string)

	/*
		_, filename, _, _ := runtime.Caller(0)
		conf, err := ioutil.ReadFile(path.Join(path.Dir(filename), "../configuration.yaml"))

		if err != nil {
			log.Errorf("ethereum config not found: %s", err.Error())
			return nil, err
		}

		err = yaml.Unmarshal(conf, &ethConfig)
		if err != nil {
			log.Errorf("ethereum config not valid: %s", err.Error())
			return nil, err
		}
	*/

	reg, err := NewRegistry(common.HexToAddress(ethConfig.RegistryAddress), client)
	if err != nil {
		log.Errorf("error initilaizing contract failed: %s", err.Error())
		return nil, err
	}

	//reg.GetVersionDetails()

	//smtct, err := NewWallet(common.HexToAddress(ethConfig.RopstenPPAddress), client)
	//if err != nil {
	//	log.Fatalf("error initilaizing contract failed: %s", err.Error())
	//}

	er := NewEthereumPriceFetcher(proxy)

	return &EthereumWallet{client, myAccount, &EthAddress{&addr}, &Service{}, reg, nil, cfg.DB, er, []func(wi.TransactionCallback){}}, nil
}

// Params - return nil to comply
func (wallet *EthereumWallet) Params() *chaincfg.Params {
	return nil
}

// GetBalance returns the balance for the wallet
func (wallet *EthereumWallet) GetBalance() (*big.Int, error) {
	return wallet.client.GetBalance(wallet.account.Address())
}

// GetUnconfirmedBalance returns the unconfirmed balance for the wallet
func (wallet *EthereumWallet) GetUnconfirmedBalance() (*big.Int, error) {
	return wallet.client.GetUnconfirmedBalance(wallet.account.Address())
}

// Transfer will transfer the amount from this wallet to the spec address
func (wallet *EthereumWallet) Transfer(to string, value *big.Int) (common.Hash, error) {
	toAddress := common.HexToAddress(to)
	return wallet.client.Transfer(wallet.account, toAddress, value)
}

// Start will start the wallet daemon
func (wallet *EthereumWallet) Start() {
	// start the ticker to check for pending txn rcpts
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			// get the pending txns
			txns, err := wallet.db.Txns().GetAll(true)
			if err != nil {
				continue
			}
			for _, txn := range txns {
				hash := common.HexToHash(txn.Txid)
				go wallet.CheckTxnRcpt(&hash, txn.Bytes)
			}
		}
	}()
}

// CurrencyCode returns ETH
func (wallet *EthereumWallet) CurrencyCode() string {
	return "ETH"
}

// IsDust Check if this amount is considered dust - 10000 wei
func (wallet *EthereumWallet) IsDust(amount int64) bool {
	return amount < 10000
}

// MasterPrivateKey - Get the master private key
func (wallet *EthereumWallet) MasterPrivateKey() *hd.ExtendedKey {
	return hd.NewExtendedKey([]byte{0x00, 0x00, 0x00, 0x00}, wallet.account.privateKey.D.Bytes(),
		wallet.account.address.Bytes(), wallet.account.address.Bytes(), 0, 0, true)
}

// MasterPublicKey - Get the master public key
func (wallet *EthereumWallet) MasterPublicKey() *hd.ExtendedKey {
	publicKey := wallet.account.privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}

	publicKeyBytes := crypto.FromECDSAPub(publicKeyECDSA)
	return hd.NewExtendedKey([]byte{0x00, 0x00, 0x00, 0x00}, publicKeyBytes,
		wallet.account.address.Bytes(), wallet.account.address.Bytes(), 0, 0, false)
}

// ChildKey Generate a child key using the given chaincode. The key is used in multisig transactions.
// For most implementations this should just be child key 0.
func (wallet *EthereumWallet) ChildKey(keyBytes []byte, chaincode []byte, isPrivateKey bool) (*hd.ExtendedKey, error) {

	parentFP := []byte{0x00, 0x00, 0x00, 0x00}
	version := []byte{0x04, 0x88, 0xad, 0xe4} // starts with xprv
	if !isPrivateKey {
		version = []byte{0x04, 0x88, 0xb2, 0x1e}
	}
	/*
		hdKey := hd.NewExtendedKey(
			version,
			keyBytes,
			chaincode,
			parentFP,
			0,
			0,
			isPrivateKey)
		return hdKey.Child(0)
	*/

	return hd.NewExtendedKey(version, keyBytes, chaincode, parentFP, 0, 0, isPrivateKey), nil
}

// CurrentAddress - Get the current address for the given purpose
func (wallet *EthereumWallet) CurrentAddress(purpose wi.KeyPurpose) btcutil.Address {
	return *wallet.address
}

// NewAddress - Returns a fresh address that has never been returned by this function
func (wallet *EthereumWallet) NewAddress(purpose wi.KeyPurpose) btcutil.Address {
	return *wallet.address
}

// DecodeAddress - Parse the address string and return an address interface
func (wallet *EthereumWallet) DecodeAddress(addr string) (btcutil.Address, error) {
	ethAddr := common.HexToAddress(addr)
	//if wallet.HasKey(EthAddress{&ethAddr}) {
	//		return *wallet.address, nil
	//	}
	return EthAddress{&ethAddr}, nil
}

// ScriptToAddress - ?
func (wallet *EthereumWallet) ScriptToAddress(script []byte) (btcutil.Address, error) {
	return wallet.address, nil
}

// HasKey - Returns if the wallet has the key for the given address
func (wallet *EthereumWallet) HasKey(addr btcutil.Address) bool {
	if !util.IsValidAddress(addr.String()) {
		return false
	}
	return wallet.account.Address().String() == addr.String()
}

// Balance - Get the confirmed and unconfirmed balances
func (wallet *EthereumWallet) Balance() (confirmed, unconfirmed int64) {
	var balance, ucbalance int64
	bal, err := wallet.GetBalance()
	if err == nil {
		balance = bal.Int64()
	}
	ucbal, err := wallet.GetUnconfirmedBalance()
	if err == nil {
		ucbalance = ucbal.Int64()
	}
	ucb := int64(0)
	if ucbalance > balance {
		ucb = ucbalance - balance
	}
	return balance, ucb
}

// Transactions - Returns a list of transactions for this wallet
func (wallet *EthereumWallet) Transactions() ([]wi.Txn, error) {
	txns, err := wallet.client.eClient.NormalTxByAddress(wallet.account.Address().String(), nil, nil,
		1, 0, true)
	if err != nil {
		return []wi.Txn{}, err
	}

	ret := []wi.Txn{}
	for _, t := range txns {
		status := wi.StatusConfirmed
		if t.IsError != 0 {
			status = wi.StatusError
		}
		tnew := wi.Txn{
			Txid:          t.Hash,
			Value:         t.Value.Int().Int64(),
			Height:        int32(t.BlockNumber),
			Timestamp:     t.TimeStamp.Time(),
			WatchOnly:     false,
			Confirmations: int64(t.Confirmations),
			Status:        wi.StatusCode(status),
			Bytes:         []byte(t.Input),
		}
		ret = append(ret, tnew)
	}

	return ret, nil
}

// GetTransaction - Get info on a specific transaction
func (wallet *EthereumWallet) GetTransaction(txid chainhash.Hash) (wi.Txn, error) {
	tx, _, err := wallet.client.GetTransaction(common.HexToHash(txid.String()))
	if err != nil {
		return wi.Txn{}, err
	}

	chainID, err := wallet.client.NetworkID(context.Background())
	if err != nil {
		return wi.Txn{}, err
	}

	msg, err := tx.AsMessage(types.NewEIP155Signer(chainID)) // HomesteadSigner{})
	if err != nil {
		return wi.Txn{}, err
	}

	return wi.Txn{
		Txid:        tx.Hash().Hex(),
		Value:       tx.Value().Int64(),
		Height:      0,
		Timestamp:   time.Now(),
		WatchOnly:   false,
		Bytes:       tx.Data(),
		ToAddress:   tx.To().String(),
		FromAddress: msg.From().Hex(),
	}, nil
}

// ChainTip - Get the height and best hash of the blockchain
func (wallet *EthereumWallet) ChainTip() (uint32, chainhash.Hash) {
	num, hash, err := wallet.client.GetLatestBlock()
	h, _ := chainhash.NewHashFromStr("")
	if err != nil {
		return 0, *h
	}
	h, _ = chainhash.NewHashFromStr(hash.Hex()[2:])
	return num, *h
}

// GetFeePerByte - Get the current fee per byte
func (wallet *EthereumWallet) GetFeePerByte(feeLevel wi.FeeLevel) uint64 {
	return 0
}

// Spend - Send ether to an external wallet
func (wallet *EthereumWallet) Spend(amount int64, addr btcutil.Address, feeLevel wi.FeeLevel, referenceID string) (*chainhash.Hash, error) {

	var hash common.Hash
	var h *chainhash.Hash
	var err error
	actualRecipient := addr

	if referenceID == "" {
		// no referenceID means this is a direct transfer
		hash, err = wallet.Transfer(addr.String(), big.NewInt(amount))
	} else {
		// this is a spend which means it has to be linked to an order
		// specified using the referenceID

		//twoMinutes, _ := time.ParseDuration("2m")

		// check if the addr is a multisig addr
		scripts, err := wallet.db.WatchedScripts().GetAll()
		if err != nil {
			return nil, err
		}
		isScript := false
		addrEth := common.HexToAddress(addr.String())
		key := addrEth.Bytes()
		fmt.Println("in spend ...")
		fmt.Println("key is : ", key)
		redeemScript := []byte{}

		for _, script := range scripts {
			fmt.Println("script is : ", script)
			fmt.Println("partial script : ", script[:common.AddressLength])
			if bytes.Equal(key, script[:common.AddressLength]) {
				isScript = true
				redeemScript = script[common.AddressLength:]
				break
			}
		}

		if isScript {
			fmt.Println("we have escrow address")
			ethScript, err := DeserializeEthScript(redeemScript)
			if err != nil {
				return nil, err
			}
			fmt.Println("ethScript : ")
			spew.Dump(ethScript)
			//actualRecipient = EthAddress{address: &ethScript.Seller}
			hash, err = wallet.callAddTransaction(ethScript, big.NewInt(amount))
			if err != nil {
				log.Errorf("error call add txn: %v", err)
			}
		} else {
			hash, err = wallet.Transfer(addr.String(), big.NewInt(amount))
		}

		if err != nil {
			return nil, err
		}
		start := time.Now()
		flag := false
		var rcpt *types.Receipt
		for !flag {
			rcpt, err = wallet.client.TransactionReceipt(context.Background(), hash)
			if rcpt != nil {
				flag = true
			}
			if time.Since(start).Seconds() > 120 {
				flag = true
			}
			if err != nil {
				log.Errorf("error fetching txn rcpt: %v", err)
			}
		}
		if rcpt != nil {
			// good. so the txn has been processed but we have to account for failed
			// but valid txn like some contract condition causing revert
			if rcpt.Status > 0 {
				// all good to update order state
				go wallet.CallTransactionListeners(wallet.createTxnCallback(hash.Hex(), referenceID, actualRecipient, amount, time.Now()))
			} else {
				// there was some error processing this txn
				nonce, err := wallet.client.GetTxnNonce(hash.Hex())
				if err == nil {
					data, err := SerializePendingTxn(PendingTxn{
						TxnID:   hash,
						Amount:  amount,
						OrderID: referenceID,
						Nonce:   nonce,
						From:    wallet.address.EncodeAddress(),
						To:      actualRecipient.EncodeAddress(),
					})
					if err == nil {
						wallet.db.Txns().Put(data, hash.Hex(), 0, 0, time.Now(), true)
					}
				}

				return nil, errors.New("transaction pending")
			}
		}
	}

	if err == nil {
		h, err = chainhash.NewHashFromStr(hash.Hex()[2:])
	}
	return h, err
}

func (wallet *EthereumWallet) createTxnCallback(txID, orderID string, toAddress btcutil.Address, value int64, bTime time.Time) wi.TransactionCallback {
	output := wi.TransactionOutput{
		Address: toAddress,
		Value:   value,
		Index:   1,
		OrderID: orderID,
	}

	input := wi.TransactionInput{
		//OutpointHash:  []byte(txID[:32]),
		//OutpointIndex: 1,
		//LinkedAddress: toAddress,
		//Value:         value,
		//OrderID:       orderID,
	}

	return wi.TransactionCallback{
		Txid:      txID[2:],
		Outputs:   []wi.TransactionOutput{output},
		Inputs:    []wi.TransactionInput{input},
		Height:    1,
		Timestamp: time.Now(),
		Value:     value,
		WatchOnly: false,
		BlockTime: bTime,
	}
}

func (wallet *EthereumWallet) CallTransactionListeners(txnCB wi.TransactionCallback) {
	for _, l := range wallet.listeners {
		go l(txnCB)
	}
}

// CheckTxnRcpt check the txn rcpt status
func (wallet *EthereumWallet) CheckTxnRcpt(hash *common.Hash, data []byte) (*common.Hash, error) {

	var rcpt *types.Receipt
	pTxn, err := DeserializePendingTxn(data)
	if err != nil {
		return nil, err
	}

	rcpt, err = wallet.client.TransactionReceipt(context.Background(), *hash)
	if err != nil {
		log.Errorf("error fetching txn rcpt: %v", err)
	}

	if rcpt != nil {
		// good. so the txn has been processed but we have to account for failed
		// but valid txn like some contract condition causing revert
		if rcpt.Status > 0 {
			// all good to update order state
			chash, err := chainhash.NewHashFromStr((*hash).Hex()[2:])
			if err != nil {
				return nil, err
			}
			wallet.db.Txns().Delete(chash)
			toAddr := common.HexToAddress(pTxn.To)
			go wallet.CallTransactionListeners(
				wallet.createTxnCallback(hash.Hex(), pTxn.OrderID, EthAddress{&toAddr},
					pTxn.Amount, time.Now()))
		}
	}

	return hash, nil

}

// BumpFee - Bump the fee for the given transaction
func (wallet *EthereumWallet) BumpFee(txid chainhash.Hash) (*chainhash.Hash, error) {
	return chainhash.NewHashFromStr(txid.String())
}

// EstimateFee - Calculates the estimated size of the transaction and returns the total fee for the given feePerByte
func (wallet *EthereumWallet) EstimateFee(ins []wi.TransactionInput, outs []wi.TransactionOutput, feePerByte uint64) uint64 {
	sum := big.NewInt(0)
	for _, out := range outs {
		gas, err := wallet.client.EstimateTxnGas(wallet.account.Address(),
			common.HexToAddress(out.Address.String()), big.NewInt(out.Value))
		if err != nil {
			return sum.Uint64()
		}
		sum.Add(sum, gas)
	}
	return sum.Uint64()
}

// EstimateSpendFee - Build a spend transaction for the amount and return the transaction fee
func (wallet *EthereumWallet) EstimateSpendFee(amount int64, feeLevel wi.FeeLevel) (uint64, error) {
	gas, err := wallet.client.EstimateGasSpend(wallet.account.Address(), big.NewInt(amount))
	return gas.Uint64(), err
}

// SweepAddress - Build and broadcast a transaction that sweeps all coins from an address. If it is a p2sh multisig, the redeemScript must be included
func (wallet *EthereumWallet) SweepAddress(utxos []wi.TransactionInput, address *btcutil.Address, key *hd.ExtendedKey, redeemScript *[]byte, feeLevel wi.FeeLevel) (*chainhash.Hash, error) {
	fmt.Println("in sweep address ...")
	fmt.Println(utxos)
	fmt.Println("addr : ", address)
	fmt.Println("key : ")
	fmt.Println(key)

	outs := []wi.TransactionOutput{}
	for i, in := range utxos {
		out := wi.TransactionOutput{
			Address: wallet.address,
			Value:   in.Value,
			Index:   uint32(i),
			OrderID: in.OrderID,
		}
		outs = append(outs, out)
	}

	sigs, err := wallet.CreateMultisigSignature([]wi.TransactionInput{}, outs, key, *redeemScript, 1)
	if err != nil {
		return nil, err
	}

	data, err := wallet.Multisign([]wi.TransactionInput{}, outs, sigs, []wi.Signature{}, *redeemScript, 1, false)
	if err != nil {
		return nil, err
	}

	tx := types.Transaction{}

	err = tx.UnmarshalJSON(data)
	if err != nil {
		return nil, err
	}

	hash := tx.Hash()

	return chainhash.NewHashFromStr(hash.Hex()[2:])
}

// ExchangeRates - return the exchangerates
func (wallet *EthereumWallet) ExchangeRates() wi.ExchangeRates {
	return wallet.exchangeRates
}

func (wallet *EthereumWallet) callAddTransaction(script EthRedeemScript, value *big.Int) (common.Hash, error) {
	fmt.Println("in call add txn")

	h := common.BigToHash(big.NewInt(0))

	// call registry to get the deployed address for the escrow ct
	fromAddress := wallet.account.Address()
	nonce, err := wallet.client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		log.Fatal(err)
	}
	gasPrice, err := wallet.client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	auth := bind.NewKeyedTransactor(wallet.account.privateKey)

	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = value      // in wei
	auth.GasLimit = 4000000 // in units
	auth.GasPrice = gasPrice

	//redeemScript, err := SerializeEthScript(script)
	//if err != nil {
	//	return h, err
	//}

	shash, _, err := GenScriptHash(script)
	if err != nil {
		return h, err
	}

	smtct, err := NewEscrow(script.MultisigAddress, wallet.client)
	if err != nil {
		log.Fatalf("error initilaizing contract failed: %s", err.Error())
	}

	///smtct.CalculateRedeemScriptHash()

	var tx *types.Transaction

	//if script.Threshold == 1 {
	tx, err = smtct.AddTransaction(auth, script.Buyer, script.Seller,
		script.Moderator, script.Threshold,
		script.Timeout, shash, script.TxnID)
	//} else {
	//	tx, err = smtct.AddTransaction(auth, script.Buyer, script.Seller,
	//		script.Moderator, script.Threshold,
	//		script.Timeout, shash, script.TxnID)
	//}

	fmt.Println("the ret txn : ")
	spew.Dump(tx)

	fmt.Println("and err : ", err)

	if err == nil {
		h = tx.Hash()
	}

	return h, err

}

// GenerateMultisigScript - Generate a multisig script from public keys. If a timeout is included the returned script should be a timelocked escrow which releases using the timeoutKey.
func (wallet *EthereumWallet) GenerateMultisigScript(keys []hd.ExtendedKey, threshold int, timeout time.Duration, timeoutKey *hd.ExtendedKey) (btcutil.Address, []byte, error) {
	if uint32(timeout.Hours()) > 0 && timeoutKey == nil {
		return nil, nil, errors.New("timeout key must be non nil when using an escrow timeout")
	}

	if len(keys) < threshold {
		return nil, nil, fmt.Errorf("unable to generate multisig script with "+
			"%d required signatures when there are only %d public "+
			"keys available", threshold, len(keys))
	}

	if len(keys) < 2 {
		return nil, nil, fmt.Errorf("unable to generate multisig script with "+
			"%d required signatures when there are only %d public "+
			"keys available", threshold, len(keys))
	}

	var ecKeys []common.Address
	for _, key := range keys {
		ecKey, err := key.ECPubKey()
		if err != nil {
			return nil, nil, err
		}
		ePubkey := ecKey.ToECDSA()
		ecKeys = append(ecKeys, crypto.PubkeyToAddress(*ePubkey))
	}
	fmt.Println("after parsing the pub keys : ", ecKeys)

	ver, err := wallet.registry.GetRecommendedVersion(nil, "escrow")
	if err != nil {
		log.Fatal(err)
	}

	if util.IsZeroAddress(ver.Implementation) {
		return nil, nil, errors.New("no escrow contract available")
	}

	builder := EthRedeemScript{}

	builder.TxnID = common.BytesToAddress(util.ExtractChaincode(&keys[0]))
	builder.Timeout = uint32(timeout.Hours())
	builder.Threshold = uint8(threshold)
	builder.Buyer = ecKeys[0]
	builder.Seller = ecKeys[1]
	builder.MultisigAddress = ver.Implementation

	fmt.Println("eth redeemscript : ")
	spew.Dump(builder)

	if threshold > 1 {
		builder.Moderator = ecKeys[2]
	}
	switch threshold {
	case 1:
		{
			// Seller is offline
		}
	case 2:
		{
			// Moderated payment
		}
	default:
		{
			// handle this
		}
	}

	redeemScript, err := SerializeEthScript(builder)
	if err != nil {
		return nil, nil, err
	}

	hash := sha3.NewKeccak256()
	hash.Write(redeemScript)
	addr := common.HexToAddress(hexutil.Encode(hash.Sum(nil)[:]))
	retAddr := EthAddress{&addr}

	scriptKey := append(addr.Bytes(), redeemScript...)
	fmt.Println("in genmultisig ....")
	fmt.Println("addr : ", addr.Bytes())
	fmt.Println("scriptkey : ", scriptKey)
	wallet.db.WatchedScripts().Put(scriptKey)

	return retAddr, redeemScript, nil
}

// CreateMultisigSignature - Create a signature for a multisig transaction
func (wallet *EthereumWallet) CreateMultisigSignature(ins []wi.TransactionInput, outs []wi.TransactionOutput, key *hd.ExtendedKey, redeemScript []byte, feePerByte uint64) ([]wi.Signature, error) {

	fmt.Println("in create multisig ... ")
	fmt.Println("ins : ")
	fmt.Println(ins)
	fmt.Println("outs :")
	fmt.Println(outs)

	wallet.printKeys()

	payouts := []wi.TransactionOutput{}
	//delta1 := int64(0)
	//delta2 := int64(0)
	difference := int64(0)

	if len(ins) > 0 {
		totalVal := ins[0].Value
		//num := len(outs)
		outVal := int64(0)
		for _, out := range outs {
			outVal += out.Value
		}
		if totalVal != outVal {
			if totalVal < outVal {
				return nil, errors.New("payout greater than initial amount")
			}
			difference = totalVal - outVal
			//delta1 = difference / int64(num)
		}
		//delta2 = totalVal - (outVal + (int64(num) * delta1))
	}

	rScript, err := DeserializeEthScript(redeemScript)
	if err != nil {
		return nil, err
	}

	indx := []int{}

	for i, out := range outs {
		if out.Address.String() != rScript.Moderator.Hex() {
			indx = append(indx, i)
		}
		p := wi.TransactionOutput{
			Address: out.Address,
			Value:   out.Value,
			Index:   out.Index,
			OrderID: out.OrderID,
		}
		payouts = append(payouts, p)
	}

	if len(indx) > 0 {
		diff := difference / int64(len(indx))
		delta := difference - (diff * int64(len(indx)))
		for _, i := range indx {
			payouts[i].Value = payouts[i].Value + diff
		}
		payouts[indx[0]].Value = payouts[indx[0]].Value + delta
	}

	sort.Slice(payouts, func(i, j int) bool {
		return strings.Compare(payouts[i].Address.String(), payouts[j].Address.String()) == -1
	})

	var sigs []wi.Signature

	payables := make(map[string]*big.Int)
	for _, out := range payouts {
		if out.Value <= 0 {
			continue
		}
		val := big.NewInt(out.Value)
		if p, ok := payables[out.Address.String()]; ok {
			sum := big.NewInt(0)
			sum.Add(val, p)
			payables[out.Address.String()] = sum
		} else {
			payables[out.Address.String()] = val
		}
	}

	fmt.Println("payables : ")
	spew.Dump(payables)

	//destinations := []common.Address{}
	//amounts := []*big.Int{}

	destArr := []byte{}
	//destStr := ""
	amountArr := []byte{}
	//amountStr := ""

	//spew.Dump(payables)

	for k, v := range payables {
		addr := common.HexToAddress(k)
		sample := [32]byte{}
		sampleDest := [32]byte{}
		copy(sampleDest[12:], addr.Bytes())
		a := make([]byte, 8)
		binary.BigEndian.PutUint64(a, v.Uint64())

		copy(sample[24:], a)
		//destinations = append(destinations, addr)
		//amounts = append(amounts, v)
		//addrStr := fmt.Sprintf("%064s", addr.String())
		//destStr = destStr + addrStr
		destArr = append(destArr, sampleDest[:]...)
		amountArr = append(amountArr, sample[:]...)
		//amnt := fmt.Sprintf("%064s", fmt.Sprintf("%x", v.Int64()))
		//amountStr = amountStr + amnt
	}

	//fmt.Println("destarr     : ", destArr)
	//fmt.Println("amountArr   : ", amountArr)

	//spew.Dump(rScript)

	shash, _, err := GenScriptHash(rScript)
	if err != nil {
		return nil, err
	}

	var txHash [32]byte
	var payloadHash [32]byte

	/*
				// Follows ERC191 signature scheme: https://github.com/ethereum/EIPs/issues/191
		        bytes32 txHash = keccak256(
		            abi.encodePacked(
		                "\x19Ethereum Signed Message:\n32",
		                keccak256(
		                    abi.encodePacked(
		                        byte(0x19),
		                        byte(0),
		                        this,
		                        destinations,
		                        amounts,
		                        scriptHash
		                    )
		                )
		            )
		        );

	*/

	payload := []byte{byte(0x19), byte(0)}
	payload = append(payload, rScript.MultisigAddress.Bytes()...)
	payload = append(payload, destArr...)
	payload = append(payload, amountArr...)
	payload = append(payload, shash[:]...)

	//script.MultisigAddress.String()[2:]

	//payloadStr := "0x19" + "00" + rScript.MultisigAddress.String()[2:] + destStr + amountStr +
	//	hashStr[2:]
	//payloadStr = payloadStr + ""

	//pHash := crypto.Keccak256([]byte(payloadStr))
	pHash := crypto.Keccak256(payload)
	copy(payloadHash[:], pHash)

	txData := []byte{byte(0x19)}
	txData = append(txData, []byte("Ethereum Signed Message:\n32")...)
	//txData = append(txData, byte(32))
	txData = append(txData, payloadHash[:]...)
	txnHash := crypto.Keccak256(txData)
	//fmt.Println("txnHash        : ", hexutil.Encode(txnHash))
	//fmt.Println("phash          : ", hexutil.Encode(payloadHash[:]))
	copy(txHash[:], txnHash)

	sig, err := crypto.Sign(txHash[:], wallet.account.privateKey)
	if err != nil {
		log.Errorf("error signing in createmultisig : %v", err)
	}
	sigs = append(sigs, wi.Signature{InputIndex: 1, Signature: sig})

	fmt.Println("the gen sigs are : ")
	spew.Dump(sigs)
	fmt.Println("and the err : ", err)

	return sigs, err
}

// Multisign - Combine signatures and optionally broadcast
func (wallet *EthereumWallet) Multisign(ins []wi.TransactionInput, outs []wi.TransactionOutput, sigs1 []wi.Signature, sigs2 []wi.Signature, redeemScript []byte, feePerByte uint64, broadcast bool) ([]byte, error) {

	fmt.Println("in multisign ....")
	fmt.Println("ins : ", ins)
	//spew.Dump(ins)
	fmt.Println("outs :", outs)
	//spew.Dump(outs)
	fmt.Println("sigs1 : ", sigs1)
	//spew.Dump(sigs1)
	fmt.Println("sigs2 :", sigs2)
	//spew.Dump(sigs2)

	//var buf bytes.Buffer

	payouts := []wi.TransactionOutput{}
	//delta1 := int64(0)
	//delta2 := int64(0)
	difference := int64(0)

	if len(ins) > 0 {
		totalVal := ins[0].Value
		//num := len(outs)
		outVal := int64(0)
		for _, out := range outs {
			outVal += out.Value
		}
		if totalVal != outVal {
			if totalVal < outVal {
				return nil, errors.New("payout greater than initial amount")
			}
			difference = totalVal - outVal
			//delta1 = difference / int64(num)
		}
		//delta2 = totalVal - (outVal + (int64(num) * delta1))
	}

	rScript, err := DeserializeEthScript(redeemScript)
	if err != nil {
		return nil, err
	}

	indx := []int{}

	for i, out := range outs {
		if out.Address.String() != rScript.Moderator.Hex() {
			indx = append(indx, i)
		}
		p := wi.TransactionOutput{
			Address: out.Address,
			Value:   out.Value,
			Index:   out.Index,
			OrderID: out.OrderID,
		}
		payouts = append(payouts, p)
	}

	if len(indx) > 0 {
		diff := difference / int64(len(indx))
		delta := difference - (diff * int64(len(indx)))
		for _, i := range indx {
			payouts[i].Value = payouts[i].Value + diff
		}
		payouts[indx[0]].Value = payouts[indx[0]].Value + delta
	}

	sort.Slice(payouts, func(i, j int) bool {
		return strings.Compare(payouts[i].Address.String(), payouts[j].Address.String()) == -1
	})

	payables := make(map[string]*big.Int)
	for _, out := range payouts {
		if out.Value <= 0 {
			continue
		}
		val := big.NewInt(out.Value)
		if p, ok := payables[out.Address.String()]; ok {
			sum := big.NewInt(0)
			sum.Add(val, p)
			payables[out.Address.String()] = sum
		} else {
			payables[out.Address.String()] = val
		}
	}

	fmt.Println("payables : ")
	spew.Dump(payables)

	rSlice := [][32]byte{} //, 2)
	sSlice := [][32]byte{} //, 2)
	vSlice := []uint8{}    //, 2)

	var r [32]byte
	var s [32]byte
	var v uint8

	if len(sigs1) > 0 && len(sigs1[0].Signature) > 0 {
		r, s, v = util.SigRSV(sigs1[0].Signature)
		rSlice = append(rSlice, r)
		sSlice = append(sSlice, s)
		vSlice = append(vSlice, v)
	}

	if len(sigs2) > 0 && len(sigs2[0].Signature) > 0 {
		r, s, v = util.SigRSV(sigs2[0].Signature)
		rSlice = append(rSlice, r)
		sSlice = append(sSlice, s)
		vSlice = append(vSlice, v)
	}

	//r = [32]byte{}
	//s = [32]byte{}
	//v = uint8(0)

	shash, _, err := GenScriptHash(rScript)
	if err != nil {
		return nil, err
	}

	smtct, err := NewEscrow(rScript.MultisigAddress, wallet.client)
	if err != nil {
		log.Fatalf("error initilaizing contract failed: %s", err.Error())
	}

	destinations := []common.Address{}
	amounts := []*big.Int{}

	for k, v := range payables {
		destinations = append(destinations, common.HexToAddress(k))
		amounts = append(amounts, v)
	}

	fromAddress := wallet.account.Address()
	nonce, err := wallet.client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		log.Fatal(err)
	}
	gasPrice, err := wallet.client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	auth := bind.NewKeyedTransactor(wallet.account.privateKey)

	auth.Nonce = big.NewInt(int64(nonce))
	auth.Value = big.NewInt(0) // in wei
	auth.GasLimit = 4000000    // in units
	auth.GasPrice = gasPrice

	var tx *types.Transaction

	tx, err = smtct.Execute(auth, vSlice, rSlice, sSlice, shash, destinations, amounts)

	fmt.Println(" smt ct exec txn : ")
	spew.Dump(tx)
	fmt.Println(" and err : ", err)

	if err != nil {
		return nil, err
	}

	ret, err := tx.MarshalJSON()

	return ret, err
}

// AddWatchedAddress - Add a script to the wallet and get notifications back when coins are received or spent from it
func (wallet *EthereumWallet) AddWatchedAddress(address btcutil.Address) error {
	// the reason eth wallet cannot use this as of now is because only the address
	// is insufficient, the redeemScript is also required
	return nil
}

// AddTransactionListener - add a txn listener
func (wallet *EthereumWallet) AddTransactionListener(callback func(wi.TransactionCallback)) {
	// add incoming txn listener using service
	wallet.listeners = append(wallet.listeners, callback)
}

// ReSyncBlockchain - Use this to re-download merkle blocks in case of missed transactions
func (wallet *EthereumWallet) ReSyncBlockchain(fromTime time.Time) {
	// use service here
}

// GetConfirmations - Return the number of confirmations and the height for a transaction
func (wallet *EthereumWallet) GetConfirmations(txid chainhash.Hash) (confirms, atHeight uint32, err error) {
	// TODO: etherscan api is being used
	// when mainnet is activated we may need a way to set the
	// url correctly
	hash := common.HexToHash(txid.String())
	urlStr := fmt.Sprintf("https://api-rinkeby.etherscan.io/api?module=proxy&action=eth_getTransactionByHash&txhash=%s", hash.String())
	res, err := http.Get(urlStr)
	if err != nil {
		return 0, 0, err
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return 0, 0, err
	}
	var s map[string]interface{}
	err = json.Unmarshal(body, &s)
	if err != nil {
		return 0, 0, err
	}

	result := s["result"].(map[string]interface{})

	d, _ := strconv.ParseInt(result["blockNumber"].(string), 0, 64)

	n, err := wallet.client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return 0, 0, err
	}

	conf := n.Number.Int64() - d

	return uint32(conf), uint32(n.Number.Int64()), nil
}

// Close will stop the wallet daemon
func (wallet *EthereumWallet) Close() {
	// stop the wallet daemon
}

// CreateAddress - used to generate a new address
func (wallet *EthereumWallet) CreateAddress() (common.Address, error) {
	fromAddress := wallet.account.Address()
	nonce, err := wallet.client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		fmt.Println(err)
	}
	addr := crypto.CreateAddress(fromAddress, nonce)
	//fmt.Println("Addr : ", addr.String())
	return addr, err
}

func (wallet *EthereumWallet) printKeys() {
	privateKeyBytes := crypto.FromECDSA(wallet.account.privateKey)
	fmt.Println("Priv Key: ", hexutil.Encode(privateKeyBytes)[2:]) // fad9c8855b740a0b7ed4c221dbad0f33a83a49cad6b3fe8d5817ac83d38b6a19

	publicKey := wallet.account.privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}

	publicKeyBytes := crypto.FromECDSAPub(publicKeyECDSA)
	fmt.Println("Pub Key: ", hexutil.Encode(publicKeyBytes)[4:]) // 9a7df67f79246283fdc93af76d4f8cdd62c4886e8cd870944e817dd0b97934fdd7719d0810951e03418205868a5c1b40b192451367f28e0088dd75e15de40c05

	address := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
	fmt.Println("Address: ", address) // 0x96216849c49358B10257cb55b28eA603c874b05E
}

// GenWallet creates a wallet
func GenWallet() {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		log.Fatal(err)
	}

	privateKeyBytes := crypto.FromECDSA(privateKey)
	fmt.Println(hexutil.Encode(privateKeyBytes)[2:]) // fad9c8855b740a0b7ed4c221dbad0f33a83a49cad6b3fe8d5817ac83d38b6a19

	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Fatal("error casting public key to ECDSA")
	}

	publicKeyBytes := crypto.FromECDSAPub(publicKeyECDSA)
	fmt.Println(hexutil.Encode(publicKeyBytes)[4:]) // 9a7df67f79246283fdc93af76d4f8cdd62c4886e8cd870944e817dd0b97934fdd7719d0810951e03418205868a5c1b40b192451367f28e0088dd75e15de40c05

	address := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()
	fmt.Println(address) // 0x96216849c49358B10257cb55b28eA603c874b05E

	hash := sha3.NewKeccak256()
	hash.Write(publicKeyBytes[1:])
	fmt.Println(hexutil.Encode(hash.Sum(nil)[12:])) // 0x96216849c49358b10257cb55b28ea603c874b05e

	fmt.Println(util.IsValidAddress(address))

}

// GenDefaultKeyStore will generate a default keystore
func GenDefaultKeyStore(passwd string) (*Account, error) {
	ks := keystore.NewKeyStore("./", keystore.StandardScryptN, keystore.StandardScryptP)

	account, err := ks.NewAccount(passwd)
	if err != nil {
		return nil, err
	}
	return NewAccountFromKeyfile(account.URL.Path, passwd)
}
