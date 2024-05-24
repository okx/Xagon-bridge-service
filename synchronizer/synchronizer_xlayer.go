package synchronizer

import (
	"context"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/0xPolygonHermez/zkevm-bridge-service/bridgectrl/pb"
	"github.com/0xPolygonHermez/zkevm-bridge-service/estimatetime"
	"github.com/0xPolygonHermez/zkevm-bridge-service/etherman"
	"github.com/0xPolygonHermez/zkevm-bridge-service/log"
	"github.com/0xPolygonHermez/zkevm-bridge-service/metrics"
	"github.com/0xPolygonHermez/zkevm-bridge-service/pushtask"
	"github.com/0xPolygonHermez/zkevm-bridge-service/server/tokenlogoinfo"
	"github.com/0xPolygonHermez/zkevm-bridge-service/utils"
	"github.com/jackc/pgx/v4"
)

func (s *ClientSynchronizer) beforeProcessDeposit(deposit *etherman.Deposit) {
	// If the deposit is USDC LxLy message, extract the user address from the metadata
	if deposit.LeafType == uint8(utils.LeafTypeMessage) && utils.IsUSDCContractAddress(deposit.OriginalAddress) {
		deposit.DestContractAddress = deposit.DestinationAddress
		deposit.DestinationAddress, _ = utils.DecodeUSDCBridgeMetadata(deposit.Metadata)
	}
}

func (s *ClientSynchronizer) afterProcessDeposit(deposit *etherman.Deposit, depositID uint64, dbTx pgx.Tx) error {
	// Add the deposit to Redis for L1
	if deposit.NetworkID == 0 {
		err := s.redisStorage.AddBlockDeposit(context.Background(), deposit)
		if err != nil {
			log.Errorf("networkID: %d, failed to add block deposit to Redis, BlockNumber: %d, Deposit: %+v, err: %s", s.networkID, deposit.BlockNumber, deposit, err)
			rollbackErr := s.storage.Rollback(s.ctx, dbTx)
			if rollbackErr != nil {
				log.Errorf("networkID: %d, error rolling back state to store block. BlockNumber: %v, rollbackErr: %v, err: %s",
					s.networkID, deposit.BlockNumber, rollbackErr, err.Error())
				return rollbackErr
			}
			return err
		}
	}

	// Original address is needed for message allow list check, but it may be changed when we replace USDC info
	origAddress := deposit.OriginalAddress
	// Replace the USDC info here so that the metrics can report the correct token info
	utils.ReplaceUSDCDepositInfo(deposit, true)

	// Notify FE about a new deposit
	go func() {
		if s.messagePushProducer == nil {
			log.Errorf("kafka push producer is nil, so can't push tx status change msg!")
			return
		}
		if deposit.LeafType != uint8(utils.LeafTypeAsset) {
			if !utils.IsUSDCContractAddress(origAddress) {
				log.Infof("transaction is not asset, so skip push update change, hash: %v", deposit.TxHash)
				return
			}
		}
		rollupWorkId := utils.GetRollupNetworkId()
		if deposit.NetworkID != rollupWorkId && deposit.DestinationNetwork != rollupWorkId {
			log.Infof("transaction is not x layer, so skip push msg and filter large tx, hash: %v", deposit.TxHash)
			return
		}

		transaction := &pb.Transaction{
			FromChain:       uint32(deposit.NetworkID),
			ToChain:         uint32(deposit.DestinationNetwork),
			BridgeToken:     deposit.OriginalAddress.Hex(),
			TokenAmount:     deposit.Amount.String(),
			EstimateTime:    s.getEstimateTimeForDepositCreated(deposit.NetworkID),
			Time:            uint64(deposit.Time.UnixMilli()),
			TxHash:          deposit.TxHash.String(),
			Id:              depositID,
			Index:           uint64(deposit.DepositCount),
			Status:          uint32(pb.TransactionStatus_TX_CREATED),
			BlockNumber:     deposit.BlockNumber,
			DestAddr:        deposit.DestinationAddress.Hex(),
			FromChainId:     utils.GetChainIdByNetworkId(deposit.NetworkID),
			ToChainId:       utils.GetChainIdByNetworkId(deposit.DestinationNetwork),
			GlobalIndex:     s.getGlobalIndex(deposit).String(),
			LeafType:        uint32(deposit.LeafType),
			OriginalNetwork: uint32(deposit.OriginalNetwork),
		}
		transactionMap := make(map[string][]*pb.Transaction)
		chainId := utils.GetChainIdByNetworkId(deposit.OriginalNetwork)
		logoCacheKey := tokenlogoinfo.GetTokenLogoMapKey(transaction.GetBridgeToken(), chainId)
		transactionMap[logoCacheKey] = append(transactionMap[logoCacheKey], transaction)
		tokenlogoinfo.FillLogoInfos(s.ctx, s.redisStorage, transactionMap)
		err := s.messagePushProducer.PushTransactionUpdate(transaction)
		if err != nil {
			log.Errorf("PushTransactionUpdate error: %v", err)
		}
		// filter and cache large transactions
		s.filterLargeTransaction(s.ctx, transaction, uint(chainId))
	}()

	metrics.RecordOrder(uint32(deposit.NetworkID), uint32(deposit.DestinationNetwork), uint32(deposit.LeafType), uint32(deposit.OriginalNetwork), deposit.OriginalAddress, deposit.Amount)
	return nil
}
func (s *ClientSynchronizer) filterLargeTransaction(ctx context.Context, transaction *pb.Transaction, chainId uint) {
	if transaction.LogoInfo == nil {
		log.Infof("failed to get logo info, so skip filter large transaction, tx: %v", transaction.GetTxHash())
		return
	}
	symbolInfo := &pb.SymbolInfo{
		ChainId: uint64(chainId),
		Address: transaction.BridgeToken,
	}
	priceInfos, err := s.redisStorage.GetCoinPrice(ctx, []*pb.SymbolInfo{symbolInfo})
	if err != nil || len(priceInfos) == 0 {
		log.Errorf("not find coin price for coin: %v, chain: %v, so skip monitor large tx: %v", symbolInfo.Address, symbolInfo.ChainId, transaction.GetTxHash())
		return
	}
	num, err := strconv.ParseInt(transaction.GetTokenAmount(), 10, 64)
	if err != nil {
		log.Errorf("failed convert coin amount to unit, err: %v, so skip monitor large tx: %v", err, transaction.GetTxHash())
		return
	}
	tokenAmount := float64(uint64(num)) / math.Pow10(int(transaction.GetLogoInfo().Decimal))
	usdAmount := priceInfos[0].Price * tokenAmount
	if usdAmount < math.Float64frombits(s.cfg.LargeTxUsdLimit) {
		log.Infof("tx usd amount less than limit, so skip, tx usd amount: %v, tx: %v", usdAmount, transaction.GetTxHash())
		return
	}
	s.freshLargeTxCache(ctx, transaction, chainId, tokenAmount, usdAmount)
}

func (s *ClientSynchronizer) freshLargeTxCache(ctx context.Context, transaction *pb.Transaction, chainId uint, tokenAmount float64, usdAmount float64) {
	largeTxInfo := &pb.LargeTxInfo{
		ChainId:   uint64(chainId),
		Symbol:    transaction.LogoInfo.Symbol,
		Amount:    tokenAmount,
		UsdAmount: usdAmount,
		Hash:      transaction.TxHash,
		Address:   transaction.DestAddr,
	}
	err := s.redisStorage.AddLargeTransaction(ctx, utils.GetLargeTxRedisKeySuffix(uint(transaction.ToChain), utils.OpWrite), largeTxInfo)
	if err != nil {
		log.Errorf("failed set large tx cache for tx: %v, err: %v", transaction.GetTxHash(), err)
	}
	delKey := utils.GetLargeTxRedisKeySuffix(uint(transaction.ToChain), utils.OpDel)
	// delete the cache before 2 days
	err = s.redisStorage.DelLargeTransactions(ctx, delKey)
	if err != nil {
		log.Errorf("failed del large tx cache for key: %v, err: %v", delKey, err)
	}
}

func (s *ClientSynchronizer) getEstimateTimeForDepositCreated(networkId uint) uint32 {
	if networkId == 0 {
		return estimatetime.GetDefaultCalculator().Get(networkId)
	}
	return uint32(pushtask.GetAvgCommitDuration(s.ctx, s.redisStorage))
}

func (s *ClientSynchronizer) afterProcessClaim(claim *etherman.Claim) error {
	// Notify FE that the tx has been claimed
	go func() {
		if s.messagePushProducer == nil {
			log.Errorf("kafka push producer is nil, so can't push tx status change msg!")
			return
		}

		originNetwork := uint(0)
		if !claim.MainnetFlag {
			originNetwork = uint(claim.RollupIndex + 1)
		}

		// Retrieve deposit transaction info
		deposit, err := s.storage.GetDeposit(s.ctx, claim.Index, originNetwork, nil)
		if err != nil {
			log.Errorf("push message: GetDeposit error: %v", err)
			return
		}
		if deposit.LeafType != uint8(utils.LeafTypeAsset) {
			if !utils.IsUSDCContractAddress(deposit.OriginalAddress) {
				log.Infof("transaction is not asset, so skip push update change, hash: %v", deposit.TxHash)
				return
			}
		}
		err = s.messagePushProducer.PushTransactionUpdate(&pb.Transaction{
			FromChain:   uint32(deposit.NetworkID),
			ToChain:     uint32(deposit.DestinationNetwork),
			TxHash:      deposit.TxHash.String(),
			Index:       uint64(deposit.DepositCount),
			Status:      uint32(pb.TransactionStatus_TX_CLAIMED),
			ClaimTxHash: claim.TxHash.Hex(),
			ClaimTime:   uint64(claim.Time.UnixMilli()),
			DestAddr:    deposit.DestinationAddress.Hex(),
			GlobalIndex: s.getGlobalIndex(deposit).String(),
		})
		if err != nil {
			log.Errorf("PushTransactionUpdate error: %v", err)
		}
	}()
	return nil
}

func (s *ClientSynchronizer) getGlobalIndex(deposit *etherman.Deposit) *big.Int {
	isMainnet := deposit.NetworkID == 0
	rollupIndex := s.rollupID - 1
	return etherman.GenerateGlobalIndex(isMainnet, rollupIndex, deposit.DepositCount)
}

// recordLatestBlockNum continuously records the latest block number to prometheus metrics
func (s *ClientSynchronizer) recordLatestBlockNum() {
	log.Debugf("Start recordLatestBlockNum")
	ticker := time.NewTicker(2 * time.Second) //nolint:gomnd

	for range ticker.C {
		// Get the latest block header
		header, err := s.etherMan.HeaderByNumber(s.ctx, nil)
		if err != nil {
			log.Errorf("HeaderByNumber err: %v", err)
			continue
		}
		metrics.RecordLatestBlockNum(uint32(s.networkID), header.Number.Uint64())
	}
}