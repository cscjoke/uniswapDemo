package main

import (
	"context"
	"encoding/hex"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	coin "github.com/pefish/go-coin-eth"
	go_decimal "github.com/pefish/go-decimal"
	logger "github.com/pefish/go-logger"
	"github.com/pkg/errors"
)

var (
	swapExactTokensForTokensMethodID = "0x38ed1739"
	// 节点地址 infura访问太快会拒绝访问
	rpcURL = ""
	// uniswap 路由合约地址
	uniRouterContractAddress = "0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D"
	// weth合约地址 tokens for eth和 eth for tokens中需要用到
	wEthContractAddress = common.HexToAddress("0xc778417e063141139fce010982780140aa0cd5ab")

	// 工厂合约地址 获取交易对地址时用到 也可通过接口获取
	factory = "0x5c69bee701ef814a2b6a3edd4b1652cb9cc5aa6f"

	// 如果是代币之间兑换需要提供双方代币合约地址
	// tokenA合约地址 交换中需要花费的代币 示例使用rinkeby 网络中的Dai
	tokenA = common.HexToAddress("0xc7ad46e0b8a400bb3c915120d284aafba8fc4735")
	// tokenA精度
	decimalTokenA = 18
	// 要花费的tokenA数量 不带精度
	spendTokenA = 1000000
	// 要花费的tokenA数量带精度
	amountTokenA = go_decimal.Decimal.Start(spendTokenA).MustShiftedBy(decimalTokenA).EndForBigInt()

	// tokenB 合约地址 需要交换的目标代币
	tokenB = common.HexToAddress("0xd1822505796C4eba9379D5a8B4141573444042c6")
	// tokenB精度
	decimalTokenB = 18

	// 能接受兑换出最少数量的代币 设置为0则表示滑点无限大，如果兑换出的代币数量少于设置的amountOutMin这交易会失败
	amountOutMin = 10

	// 钱包地址
	addr = ""
	// 钱包地址对应私钥
	pKey            = ""
	maxApprove, _   = new(big.Int).SetString("ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 16)
	decodedInitCode []byte
)

func main() {
	wallet, err := coin.NewWallet().InitRemote(coin.UrlParam{RpcUrl: rpcURL})
	if err != nil {
		logger.Logger.Error(err)
		return
	}
	wallet.SetLogger(logger.Logger)

	// 获取流动池中各个代币数量
	logger.Logger.Info(">>>>>>>> before swap")
	queryTokensAmountInLp(wallet)

	// 代币交换
	logger.Logger.Info(">>>>>>>> start swap")
	swap(wallet)

	// 获取流动池中各个代币数量
	logger.Logger.Info(">>>>>>>> after swap")
	queryTokensAmountInLp(wallet)
}

func queryTokensAmountInLp(wallet *coin.Wallet) {
	paireAddr, err := GetCreate2Address(factory, tokenA.String(), tokenB.String())
	if err != nil {
		logger.Logger.Error(err)
		return
	}

	tkn0Amount, tkn1Amount, err := getReserves(wallet, paireAddr.String())
	if err != nil {
		logger.Logger.Error(err)
		return
	}

	if new(big.Int).SetBytes(tokenA.Bytes()).Cmp(new(big.Int).SetBytes(tokenB.Bytes())) > 0 {
		logger.Logger.InfoF("in liquid pool tokenA amount is: %s, tokenB amount is: %s\n",
			go_decimal.Decimal.Start(tkn1Amount).MustUnShiftedBy(decimalTokenA).EndForString,
			go_decimal.Decimal.Start(tkn0Amount).MustUnShiftedBy(decimalTokenB).EndForString(),
		)
	} else {
		logger.Logger.InfoF("in liquid pool tokenA amount is: %s, tokenB amount is: %s\n",
			go_decimal.Decimal.Start(tkn0Amount).MustUnShiftedBy(decimalTokenA).EndForString(),
			go_decimal.Decimal.Start(tkn1Amount).MustUnShiftedBy(decimalTokenB).EndForString())
	}
}

func swap(wallet *coin.Wallet) {
	gasLimit := 300000
	gasPrice, err := wallet.SuggestGasPrice()
	if err != nil {
		logger.Logger.Error(err)
		return
	}
	logger.Logger.Info("推荐gasPrice为: ", gasPrice.String())

	logger.Logger.Info("get address balance")
	ethAmount, tokenAmount, err := getAddressBalance(wallet, addr, tokenA.String())
	if err != nil {
		logger.Logger.Error("get address balance error: ", err.Error())
		return
	}

	tokenAmountWithoutDecimal := go_decimal.Decimal.Start(tokenAmount).MustUnShiftedBy(decimalTokenA).EndForString()
	if go_decimal.Decimal.Start(tokenAmountWithoutDecimal).Lte(0) {
		logger.Logger.Error("address tokenA amount is: 0")
		return
	}
	// 检查主币余额是否够手续费
	if ethAmount.Cmp(new(big.Int).Mul(gasPrice, big.NewInt(int64(gasLimit*2)))) == -1 {
		logger.Logger.Error("地址eth余额不够支付手续费")
		return
	}

	logger.Logger.InfoF("address: %s eth amount is: %s, token amount is: %s",
		addr,
		go_decimal.Decimal.Start(ethAmount).MustUnShiftedBy(18).EndForString(),
		tokenAmountWithoutDecimal,
	)

	// 检查tokenA是否给router授权
	isApproved, err := checkApproved(wallet, addr, uniRouterContractAddress, tokenA.String())
	if err != nil {
		logger.Logger.Error("check approved failed: ", err.Error())
		return
	}

	// 未授权时 对合约做授权处理
	if !isApproved {
		logger.Logger.Info("未授权router合约, 正在授权...")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		nonce, err := wallet.RemoteRpcClient.NonceAt(ctx, common.HexToAddress(addr), nil)
		cancel()
		if err != nil {
			logger.Logger.Error("get nonce err: ", err)
			return
		}

		logger.Logger.Info("current nonce is: ", nonce)

		logger.Logger.Info("start approve")

		txHash, err := approve(wallet, pKey, tokenA.String(), uniRouterContractAddress, gasPrice, nonce, uint64(gasLimit))
		if err != nil {
			logger.Logger.Error("approve failed: ", err)
			return
		}
		wallet.WaitConfirm(txHash, time.Second*1)
		logger.Logger.Info("approve transaction is packed")
	}

	gasPrice, err = wallet.SuggestGasPrice()
	if err != nil {
		logger.Logger.Error(err)
		return
	}
	logger.Logger.Info("推荐gasPrice为: ", gasPrice.String())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	nonce, err := wallet.RemoteRpcClient.NonceAt(ctx, common.HexToAddress(addr), nil)
	cancel()
	if err != nil {
		logger.Logger.Error("get nonce err: ", err)
		return
	}

	logger.Logger.Info("start swap, current nonce: ", nonce)
	txHash, err := swapExactTokensForTokens(pKey, wallet, gasPrice, nonce, uint64(gasLimit))
	if err != nil {
		logger.Logger.Error(err)
		return
	}
	wallet.WaitConfirm(txHash, time.Second)
	logger.Logger.InfoF("swap transaction is packed, tx hash:%s \n", txHash)
}

// swapExactTokensForTokens tokenA交换tokenB
func swapExactTokensForTokens(pKey string, wallet *coin.Wallet, gasPrice *big.Int, nonce, gasLimit uint64) (txHash string, err error) {

	// 交易截止时间
	deadline := big.NewInt(time.Now().Add(time.Minute * 10).Unix())
	//交易路径
	path := []common.Address{tokenA, tokenB}
	amountOutMinWithDecimal := go_decimal.Decimal.Start(amountOutMin).MustShiftedBy(decimalTokenB).EndForBigInt()

	// 构建合约输入数据
	inputs := abi.Arguments{
		abi.Argument{Name: "amountIn", Type: coin.TypeUint256},
		abi.Argument{Name: "amountOutMin", Type: coin.TypeUint256},
		abi.Argument{Name: "path", Type: coin.TypeAddressArr},
		abi.Argument{Name: "to", Type: coin.TypeAddress},
		abi.Argument{Name: "deadline", Type: coin.TypeUint256},
	}
	params, err := wallet.PackParams(inputs, amountTokenA, amountOutMinWithDecimal, path, common.HexToAddress(addr), deadline)
	if err != nil {
		err = errors.Wrap(err, "packed params to string")
		return
	}

	opts := coin.CallMethodOpts{
		Nonce:    nonce,
		GasPrice: gasPrice,
		GasLimit: gasLimit,
	}

	tx, err := wallet.BuildCallMethodTxWithPayload(pKey, uniRouterContractAddress, swapExactTokensForTokensMethodID+params, &opts)
	if err != nil {
		err = errors.Wrap(err, "build contract transaction ")
		return
	}

	return wallet.SendRawTransaction(tx.TxHex)
}

// getAddressBalance 获取地址指定代币余额和主币余额
// contractAddress 指定代币合约地址
// address 要获取余额的地址 ethAmount 地址eth数量带精度 tokenAmount 代币余额带精度
func getAddressBalance(wallet *coin.Wallet, address, contractAddress string) (ethAmount, tokenAmount *big.Int, err error) {
	ethAmount, err = wallet.Balance(address)
	if err != nil {
		err = errors.Wrap(err, "get eth balance")
		return
	}
	tokenAmount, err = wallet.TokenBalance(contractAddress, address)
	if err != nil {
		err = errors.Wrap(err, "get token balance")
	}
	return
}

//checkApproved 判断代币是否对uniswap router合约授权
func checkApproved(wallet *coin.Wallet, address, uniRouterAddress, contractAddress string) (isApproved bool, err error) {
	res, err := wallet.CallContractConstant(contractAddress, coin.Erc20AbiStr, "allowance", nil, common.HexToAddress(address), common.HexToAddress(uniRouterAddress))
	if err != nil {
		err = errors.Wrap(err, "get approve")
		return
	}
	approved, ok := res[0].(*big.Int)
	if !ok {
		err = errors.New("approve amount is not *big.Int")
		return
	}
	logger.Logger.InfoF("approved amount is: %s, maxApprove is: %s", approved.String(), maxApprove.String())
	isApproved = approved.Cmp(maxApprove) != -1
	return
}

func approve(wallet *coin.Wallet, pKey, tokenAddress, spendAddress string, gasPrice *big.Int, nonce, gasLimit uint64) (txHash string, err error) {
	opts := coin.CallMethodOpts{
		Nonce:    nonce,
		GasPrice: gasPrice,
		GasLimit: gasLimit,
	}
	tx, err := wallet.BuildCallMethodTx(pKey, tokenAddress, coin.Erc20AbiStr, "approve", &opts, common.HexToAddress(spendAddress), maxApprove)
	if err != nil {
		errors.Wrap(err, "build call method transaction")
		return
	}

	txHash, err = wallet.SendRawTransaction(tx.TxHex)
	return
}

var getReservesAbi = `[{"constant":true,"inputs":[],"name":"getReserves","outputs":[{"internalType":"uint112","name":"_reserve0","type":"uint112"},{"internalType":"uint112","name":"_reserve1","type":"uint112"},{"internalType":"uint32","name":"_blockTimestampLast","type":"uint32"}],"payable":false,"stateMutability":"view","type":"function"}]`

//getReserves 获取交易对中各个币的数量
func getReserves(wallet *coin.Wallet, pairAddress string) (tkn0Amount, tkn1Amount *big.Int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	res, err := wallet.CallContractConstant(pairAddress, getReservesAbi, "getReserves", &bind.CallOpts{Context: ctx})
	cancel()
	if err != nil {
		err = errors.Wrap(err, "call contract")
		return
	}
	tkn0Amount, tkn1Amount = res[0].(*big.Int), res[1].(*big.Int)
	return
}

//GetCreate2Address 根据factory token0 token1合约地址计算出token0 token1交易对地址
func GetCreate2Address(factory, tokenA, tokenB string) (pairAddress common.Address, err error) {
	factoryAddr := common.HexToAddress(factory)
	tkn0, tkn1 := sortAddressess(common.HexToAddress(tokenA), common.HexToAddress(tokenB))

	msg := []byte{255}
	msg = append(msg, factoryAddr.Bytes()...)
	addrBytes := tkn0.Bytes()
	addrBytes = append(addrBytes, tkn1.Bytes()...)
	msg = append(msg, crypto.Keccak256(addrBytes)...)

	if len(decodedInitCode) == 0 {
		// initCodeHash在uniswap上是固定值
		b, err1 := hex.DecodeString("96e8ac4277198ff8b6f785478aa9a39f403cb768dd02cbee326c3e7da348845f")
		if err1 != nil {
			err = errors.Wrap(err, "decode init code hash")
			return
		}
		decodedInitCode = b
	}

	msg = append(msg, decodedInitCode...)
	hash := crypto.Keccak256(msg)
	pairAddressBytes := big.NewInt(0).SetBytes(hash)
	pairAddressBytes = pairAddressBytes.Abs(pairAddressBytes)
	return common.BytesToAddress(pairAddressBytes.Bytes()), nil
}

func sortAddressess(token0, token1 common.Address) (tkn0 common.Address, tkn1 common.Address) {
	token0Rep := big.NewInt(0).SetBytes(token0.Bytes())
	token1Rep := big.NewInt(0).SetBytes(token1.Bytes())

	if token0Rep.Cmp(token1Rep) > 0 {
		token0, token1 = token1, token0
	}

	return token0, token1
}
