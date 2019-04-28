package eth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cmingxu/wallet-keeper/keeper"
	"github.com/cmingxu/wallet-keeper/notifier"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const PASSWORD = "password"

// At this inteval will refresh accountBalanceMap,
// If balance changed, event will send out to any receiver.
var AccountBalanceWatcherInterval = time.Second * 20

var ErrNotValidAccountFile = errors.New("not valid account file")
var ErrNotDirectory = errors.New("not valid directory")

// address is not valid
var ErrInvalidAddress = errors.New("invalid address")

type EthAccount struct {
	account string  `json:"account"`
	address string  `json:"address"`
	balance float64 `json:"balance"`
}

type Client struct {
	l *log.Logger

	// Checkout https://github.com/ethereum/go-ethereum/blob/master/rpc/client.go
	// for more details.
	ethRpcClient *rpc.Client

	// fs directory where to store wallet
	walletDir string

	// keystore
	store *keystore.KeyStore

	accountFilePath string
	// account/address map lock, since ethereum doesn't support account
	// we should have our own account/address map internally.
	// only with this map we can provide services for the upstream services.
	accountAddressMap  map[string]string
	accountAddressLock sync.Mutex

	// account/balance map
	accountBalanceMap  map[string]float64
	accountBalanceLock sync.Mutex

	noti *notifier.Notifier
}

func NewClient(host, walletDir, accountFilePath, logDir string) (*Client, error) {
	client := &Client{
		walletDir:          walletDir,
		accountFilePath:    accountFilePath,
		accountAddressMap:  make(map[string]string),
		accountAddressLock: sync.Mutex{},

		accountBalanceMap:  make(map[string]float64),
		accountBalanceLock: sync.Mutex{},
		noti:               notifier.New(),
	}

	// accountAddressMap initialization
	stat, err := os.Stat(client.accountFilePath)
	if err != nil {
		return nil, err
	}

	if !stat.Mode().IsRegular() {
		return nil, ErrNotValidAccountFile
	}

	err = client.loadAccountMap()
	if err != nil {
		return nil, err
	}

	// keystore initialization
	stat, err = os.Stat(walletDir)
	if err != nil {
		return nil, ErrNotDirectory
	}

	if !stat.IsDir() {
		return nil, ErrNotDirectory
	}
	client.store = keystore.NewKeyStore(walletDir, keystore.StandardScryptN, keystore.StandardScryptP)

	// rpcClient initialization
	client.ethRpcClient, err = rpc.Dial(host)
	if err != nil {
		return nil, err
	}

	// log initialization
	logPath := filepath.Join(logDir, "eth.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0755)
	if err != nil {
		return nil, err
	}

	client.l = &log.Logger{
		Out:       logFile,
		Formatter: new(log.JSONFormatter),
	}

	for account, address := range client.accountAddressMap {
		balance, err := client.getBalance(address)
		if err != nil {
			log.Debug(err)
		}
		client.accountBalanceMap[account] = balance
	}

	go client.noti.Start()
	go client.accountBalanceWatcher()

	return client, nil
}

// Ping
func (client *Client) Ping() error {
	return nil
}

// GetBlockCount
func (client *Client) GetBlockCount() (int64, error) {
	var hexHeight string
	err := client.ethRpcClient.CallContext(context.Background(), &hexHeight, "eth_blockNumber")
	if err != nil {
		return 0, err
	}

	height, err := hexutil.DecodeBig(hexHeight)
	if err != nil {
		return 0, err
	}

	return height.Int64(), nil
}

// GetAddress - default address
func (client *Client) GetAddress(account string) (string, error) {
	address, ok := client.accountAddressMap[account]
	if !ok {
		return "", keeper.ErrAccountNotFound
	}

	return address, nil
}

// Create Account
func (client *Client) CreateAccount(account string) (keeper.Account, error) {
	address, _ := client.GetAddress(account)
	if len(address) > 0 {
		return keeper.Account{}, keeper.ErrAccountExists
	}

	acc, err := client.store.NewAccount(PASSWORD)
	if err != nil {
		return keeper.Account{}, err
	}

	client.accountAddressLock.Lock()
	client.accountAddressMap[account] = acc.Address.Hex()
	client.accountAddressLock.Unlock()

	client.accountBalanceLock.Lock()
	client.accountBalanceMap[account] = 0
	client.accountBalanceLock.Unlock()

	err = client.persistAccountMap()
	if err != nil {
		return keeper.Account{}, err
	}

	return keeper.Account{
		Account: account,
		Balance: 0,
		Addresses: []string{
			acc.Address.Hex(),
		},
	}, nil
}

// GetAccountInfo
func (client *Client) GetAccountInfo(account string, minConf int) (keeper.Account, error) {
	address, found := client.accountAddressMap[account]
	if !found {
		return keeper.Account{}, keeper.ErrAccountNotFound
	}

	balance, err := client.getBalance(address)
	if err != nil {
		return keeper.Account{}, err
	}

	return keeper.Account{
		Account:   account,
		Balance:   balance,
		Addresses: []string{address},
	}, nil
}

func (client *Client) GetNewAddress(account string) (string, error) {
	return "", keeper.ErrNotSupport
}

// GetAddressesByAccount
func (client *Client) GetAddressesByAccount(account string) ([]string, error) {
	address, ok := client.accountAddressMap[account]
	if !ok {
		return []string{}, keeper.ErrAccountNotFound
	}

	return []string{address}, nil
}

// ListAccountsMinConf
func (client *Client) ListAccountsMinConf(conf int) (map[string]float64, error) {
	accounts := make(map[string]float64, len(client.accountAddressMap))
	for name, address := range client.accountAddressMap {
		balance, err := client.getBalance(address)
		if err != nil {
			client.l.Errorf("[ListAccountsMinConf] %s", err)

			accounts[name] = 0
		} else {
			accounts[name] = balance
		}
	}

	return accounts, nil
}

// SendToAddress
func (client *Client) SendToAddress(address string, amount float64) error {
	return keeper.ErrNotSupport
}

// TODO check validity of account and have sufficent balance
func (client *Client) SendFrom(account, hexToAddress string, amount float64) error {
	hexFromAddress := account
	if !common.IsHexAddress(account) {
		hexFromAddress, found := client.accountAddressMap[account]
		_ = hexFromAddress
		if !found {
			return ErrInvalidAddress
		}
	}

	if !common.IsHexAddress(hexFromAddress) {
		return ErrInvalidAddress
	}

	if !common.IsHexAddress(hexToAddress) {
		return ErrInvalidAddress
	}

	fromAddress := common.HexToAddress(hexFromAddress)
	toAddress := common.HexToAddress(hexToAddress)

	nonce, err := client.PendingNonceAt(context.Background(), fromAddress)
	if err != nil {
		log.Error(err)
		return err
	}

	value := etherToWei(amount)
	gasLimit := uint64(21000)
	gasPrice, err := client.SuggestGasPrice(context.Background())
	if err != nil {
		log.Error(err)
		return err
	}

	chainID, err := client.NetworkID(context.Background())
	if err != nil {
		log.Error(err)
		return err
	}

	tx := types.NewTransaction(nonce, toAddress, value, gasLimit, gasPrice, []byte{})
	accountStore := accounts.Account{Address: fromAddress}
	signedTx, err := client.store.SignTx(accountStore, tx, chainID)
	if err != nil {
		log.Error(err)
		return err
	}

	err = client.SendTransaction(context.Background(), signedTx)
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

// ListUnspentMin
func (client *Client) ListUnspentMin(minConf int) ([]btcjson.ListUnspentResult, error) {
	return []btcjson.ListUnspentResult{}, keeper.ErrNotSupport
}

// Move
func (client *Client) Move(from, to string, amount float64) (bool, error) {
	err := client.SendFrom(from, to, amount)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (client *Client) AddRoutes(engine *gin.Engine) {
	notificationGroup := engine.Group("/notifiers")
	// list all avaliable receivers
	notificationGroup.GET("/list", func(c *gin.Context) {
		c.JSON(http.StatusOK, client.noti.ListReceivers())
	})

	// install new receiver
	//  endpoint - http://callbackdomain.com/foo/bar
	//  retryCount - 1
	//  eventTypes - eth_balance_change_event
	notificationGroup.POST("/install", func(c *gin.Context) {
		var installParams struct {
			RetryCount uint   `json:"retryCount"`
			Endpoint   string `json:"endpoint"`
			EventTypes string `json:"eventTypes"`
		}

		if c.ShouldBind(&installParams) == nil {
			receiver := notifier.NewReceiver(
				installParams.Endpoint,
				strings.SplitN(installParams.EventTypes, ",", -1),
				installParams.RetryCount,
			)

			uuidIns, _ := uuid.NewUUID()
			client.noti.InstallReceiver(uuidIns.String(), receiver)
		}
	})

	notificationGroup.POST("/uninstall", func(c *gin.Context) {
		var uninstallParams struct {
			Name string `json:"name"`
		}

		if c.ShouldBind(&uninstallParams) == nil {
			client.noti.UninstallReceiver(uninstallParams.Name)
		}
	})
	return
}

// persistAccountMap write `accountAddressMap` into file `client.accountAddressMap`,
// `accountAddressMap` will persist into file with json format,
//
// Error - return if `client.accountFilePath` not found or write permission not right.
func (client *Client) persistAccountMap() error {
	stat, err := os.Stat(client.accountFilePath)
	if err != nil && os.IsNotExist(err) {
		return err
	}

	if !stat.Mode().IsRegular() {
		return ErrNotValidAccountFile
	}

	file, err := os.OpenFile(client.accountFilePath, os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	defer file.Close()

	return json.NewEncoder(file).Encode(client.accountAddressMap)
}

// loadAccountMap from filesystem.
func (client *Client) loadAccountMap() error {
	client.accountAddressMap = make(map[string]string)
	file, err := os.Open(client.accountFilePath)
	if err != nil {
		return err
	}
	defer file.Close()

	err = json.NewDecoder(file).Decode(&client.accountAddressMap)
	if err != nil {
		return err
	}

	return nil
}

func (client *Client) getBalance(address string) (float64, error) {
	var balance hexutil.Big
	err := client.ethRpcClient.CallContext(context.Background(), &balance,
		"eth_getBalance", common.HexToAddress(address), "latest")
	if err != nil {
		return 0, err
	}

	float64Value, _ := weiToEther(balance.ToInt()).Float64()
	return float64Value, nil
}

func (client *Client) accountBalanceWatcher() {
	ticker := time.NewTicker(AccountBalanceWatcherInterval)

	refreshFunc := func() {
		for account, balance := range client.accountBalanceMap {
			address, found := client.accountAddressMap[account]
			if found {
				newBalance, err := client.getBalance(address)
				if err != nil {
					log.Println(err)
				}

				// balance updated
				if balance != newBalance {
					event := notifier.NewEthBalanceChangeEvent(map[string]interface{}{
						"account":    account,
						"address":    address,
						"newBalance": newBalance,
						"balance":    balance,
					})
					client.noti.EventChan() <- event

					client.accountBalanceLock.Lock()
					client.accountBalanceMap[account] = newBalance
					client.accountBalanceLock.Unlock()
				}
			}
		}
	}

	refreshFunc()

	for {
		select {
		case <-ticker.C:
			refreshFunc()
		}
	}
}
