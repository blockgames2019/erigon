package stagedsync

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"runtime"
	"time"

	"github.com/c2h5oh/datasize"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/gointerfaces/remote"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/dbutils"
	"github.com/ledgerwatch/erigon/common/math"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/ethdb/privateapi"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/engineapi"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/stages/bodydownload"
	"github.com/ledgerwatch/erigon/turbo/stages/headerdownload"
	"github.com/ledgerwatch/log/v3"
)

// The number of blocks we should be able to re-org sub-second on commodity hardware.
// See https://hackmd.io/TdJtNs0dS56q-In8h-ShSg
const ShortPoSReorgThresholdBlocks = 10

type HeadersCfg struct {
	db                kv.RwDB
	hd                *headerdownload.HeaderDownload
	bodyDownload      *bodydownload.BodyDownload
	chainConfig       params.ChainConfig
	headerReqSend     func(context.Context, *headerdownload.HeaderRequest) ([64]byte, bool)
	announceNewHashes func(context.Context, []headerdownload.Announce)
	penalize          func(context.Context, []headerdownload.PenaltyItem)
	batchSize         datasize.ByteSize
	noP2PDiscovery    bool
	memoryOverlay     bool
	tmpdir            string

	blockReader   services.FullBlockReader
	forkValidator *engineapi.ForkValidator
	notifications *Notifications
}

func StageHeadersCfg(
	db kv.RwDB,
	headerDownload *headerdownload.HeaderDownload,
	bodyDownload *bodydownload.BodyDownload,
	chainConfig params.ChainConfig,
	headerReqSend func(context.Context, *headerdownload.HeaderRequest) ([64]byte, bool),
	announceNewHashes func(context.Context, []headerdownload.Announce),
	penalize func(context.Context, []headerdownload.PenaltyItem),
	batchSize datasize.ByteSize,
	noP2PDiscovery bool,
	memoryOverlay bool,
	blockReader services.FullBlockReader,
	tmpdir string,
	notifications *Notifications,
	forkValidator *engineapi.ForkValidator) HeadersCfg {
	return HeadersCfg{
		db:                db,
		hd:                headerDownload,
		bodyDownload:      bodyDownload,
		chainConfig:       chainConfig,
		headerReqSend:     headerReqSend,
		announceNewHashes: announceNewHashes,
		penalize:          penalize,
		batchSize:         batchSize,
		tmpdir:            tmpdir,
		noP2PDiscovery:    noP2PDiscovery,
		blockReader:       blockReader,
		forkValidator:     forkValidator,
		notifications:     notifications,
		memoryOverlay:     memoryOverlay,
	}
}

func SpawnStageHeaders(
	s *StageState,
	u Unwinder,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	initialCycle bool,
	test bool, // Set to true in tests, allows the stage to fail rather than wait indefinitely
) error {
	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	var blockNumber uint64
	if s == nil {
		blockNumber = 0
	} else {
		blockNumber = s.BlockNumber
	}

	notBorAndParlia := cfg.chainConfig.Bor == nil && cfg.chainConfig.Parlia == nil

	unsettledForkChoice, headHeight := cfg.hd.GetUnsettledForkChoice()
	if notBorAndParlia && unsettledForkChoice != nil { // some work left to do after unwind
		return finishHandlingForkChoice(unsettledForkChoice, headHeight, s, tx, cfg, useExternalTx)
	}

	transitionedToPoS := cfg.chainConfig.TerminalTotalDifficultyPassed
	if notBorAndParlia && !transitionedToPoS {
		var err error
		transitionedToPoS, err = rawdb.Transitioned(tx, blockNumber, cfg.chainConfig.TerminalTotalDifficulty)
		if err != nil {
			return err
		}
		if transitionedToPoS {
			cfg.hd.SetFirstPoSHeight(blockNumber)
		}
	}

	if transitionedToPoS {
		libcommon.SafeClose(cfg.hd.QuitPoWMining)
		return HeadersPOS(s, u, ctx, tx, cfg, initialCycle, test, useExternalTx)
	} else {
		return HeadersPOW(s, u, ctx, tx, cfg, initialCycle, test, useExternalTx)
	}
}

// HeadersPOS processes Proof-of-Stake requests (newPayload, forkchoiceUpdated).
// It also saves PoS headers downloaded by (*HeaderDownload)StartPoSDownloader into the DB.
func HeadersPOS(
	s *StageState,
	u Unwinder,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	initialCycle bool,
	test bool,
	useExternalTx bool,
) error {
	if initialCycle {
		// Let execution and other stages to finish before waiting for CL
		return nil
	}

	cfg.hd.SetPOSSync(true)
	syncing := cfg.hd.PosStatus() != headerdownload.Idle
	if !syncing {
		log.Info(fmt.Sprintf("[%s] Waiting for Consensus Layer...", s.LogPrefix()))
	}
	interrupt, requestId, requestWithStatus := cfg.hd.BeaconRequestList.WaitForRequest(syncing, test)

	cfg.hd.SetHeaderReader(&chainReader{config: &cfg.chainConfig, tx: tx, blockReader: cfg.blockReader})
	headerInserter := headerdownload.NewHeaderInserter(s.LogPrefix(), nil, s.BlockNumber, cfg.blockReader)

	interrupted, err := handleInterrupt(interrupt, cfg, tx, headerInserter, useExternalTx)
	if err != nil {
		return err
	}

	if interrupted {
		return nil
	}

	if requestWithStatus == nil {
		log.Warn(fmt.Sprintf("[%s] Nil beacon request. Should only happen in tests", s.LogPrefix()))
		return nil
	}

	request := requestWithStatus.Message
	requestStatus := requestWithStatus.Status

	// Decide what kind of action we need to take place
	forkChoiceMessage, forkChoiceInsteadOfNewPayload := request.(*engineapi.ForkChoiceMessage)
	cfg.hd.ClearPendingPayloadHash()
	cfg.hd.SetPendingPayloadStatus(nil)

	var payloadStatus *engineapi.PayloadStatus
	if forkChoiceInsteadOfNewPayload {
		payloadStatus, err = startHandlingForkChoice(forkChoiceMessage, requestStatus, requestId, s, u, ctx, tx, cfg, test, headerInserter)
	} else {
		payloadMessage := request.(*types.Block)
		payloadStatus, err = handleNewPayload(payloadMessage, requestStatus, requestId, s, ctx, tx, cfg, test, headerInserter)
	}

	if err != nil {
		if requestStatus == engineapi.New {
			cfg.hd.PayloadStatusCh <- engineapi.PayloadStatus{CriticalError: err}
		}
		return err
	}

	if !useExternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}

	if requestStatus == engineapi.New && payloadStatus != nil {
		if payloadStatus.Status == remote.EngineStatus_SYNCING || payloadStatus.Status == remote.EngineStatus_ACCEPTED || !useExternalTx {
			cfg.hd.PayloadStatusCh <- *payloadStatus
		} else {
			// Let the stage loop run to the end so that the transaction is committed prior to replying to CL
			cfg.hd.SetPendingPayloadStatus(payloadStatus)
		}
	}

	return nil
}

func writeForkChoiceHashes(
	forkChoice *engineapi.ForkChoiceMessage,
	s *StageState,
	tx kv.RwTx,
	cfg HeadersCfg,
) (bool, error) {
	if forkChoice.SafeBlockHash != (common.Hash{}) {
		safeIsCanonical, err := rawdb.IsCanonicalHash(tx, forkChoice.SafeBlockHash)
		if err != nil {
			return false, err
		}
		if !safeIsCanonical {
			log.Warn(fmt.Sprintf("[%s] Non-canonical SafeBlockHash", s.LogPrefix()), "forkChoice", forkChoice)
			return false, nil
		}
	}

	if forkChoice.FinalizedBlockHash != (common.Hash{}) {
		finalizedIsCanonical, err := rawdb.IsCanonicalHash(tx, forkChoice.FinalizedBlockHash)
		if err != nil {
			return false, err
		}
		if !finalizedIsCanonical {
			log.Warn(fmt.Sprintf("[%s] Non-canonical FinalizedBlockHash", s.LogPrefix()), "forkChoice", forkChoice)
			return false, nil
		}
	}

	rawdb.WriteForkchoiceHead(tx, forkChoice.HeadBlockHash)
	if forkChoice.SafeBlockHash != (common.Hash{}) {
		rawdb.WriteForkchoiceSafe(tx, forkChoice.SafeBlockHash)
	}
	if forkChoice.FinalizedBlockHash != (common.Hash{}) {
		rawdb.WriteForkchoiceFinalized(tx, forkChoice.FinalizedBlockHash)
	}

	return true, nil
}

func startHandlingForkChoice(
	forkChoice *engineapi.ForkChoiceMessage,
	requestStatus engineapi.RequestStatus,
	requestId int,
	s *StageState,
	u Unwinder,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	test bool,
	headerInserter *headerdownload.HeaderInserter,
) (*engineapi.PayloadStatus, error) {
	if cfg.memoryOverlay {
		defer cfg.forkValidator.ClearWithUnwind(tx, cfg.notifications.Accumulator, cfg.notifications.StateChangesConsumer)
	}
	headerHash := forkChoice.HeadBlockHash
	log.Debug(fmt.Sprintf("[%s] Handling fork choice", s.LogPrefix()), "headerHash", headerHash)

	currentHeadHash := rawdb.ReadHeadHeaderHash(tx)
	if currentHeadHash == headerHash { // no-op
		log.Debug(fmt.Sprintf("[%s] Fork choice no-op", s.LogPrefix()))
		cfg.hd.BeaconRequestList.Remove(requestId)
		canonical, err := writeForkChoiceHashes(forkChoice, s, tx, cfg)
		if err != nil {
			log.Warn(fmt.Sprintf("[%s] Fork choice err", s.LogPrefix()), "err", err)
			return nil, err
		}
		if canonical {
			return &engineapi.PayloadStatus{
				Status:          remote.EngineStatus_VALID,
				LatestValidHash: currentHeadHash,
			}, nil
		} else {
			return &engineapi.PayloadStatus{
				CriticalError: &privateapi.InvalidForkchoiceStateErr,
			}, nil
		}
	}

	// Header itself may already be in the snapshots, if CL starts off at much earlier state than Erigon
	header, err := cfg.blockReader.HeaderByHash(ctx, tx, headerHash)
	if err != nil {
		log.Warn(fmt.Sprintf("[%s] Fork choice err (reading header by hash %x)", s.LogPrefix(), headerHash), "err", err)
		cfg.hd.BeaconRequestList.Remove(requestId)
		return nil, err
	}

	if header == nil {
		log.Info(fmt.Sprintf("[%s] Fork choice: need to download header with hash %x", s.LogPrefix(), headerHash))
		cfg.hd.BeaconRequestList.Remove(requestId)
		if !test {
			schedulePoSDownload(requestId, headerHash, 0 /* header height is unknown, setting to 0 */, headerHash, s, cfg)
		}
		return &engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}, nil
	}

	cfg.hd.BeaconRequestList.Remove(requestId)

	headerNumber := header.Number.Uint64()

	if cfg.memoryOverlay && headerHash == cfg.forkValidator.ExtendingForkHeadHash() {
		log.Info("Flushing in-memory state")
		if err := cfg.forkValidator.FlushExtendingFork(tx); err != nil {
			return nil, err
		}
		cfg.hd.BeaconRequestList.Remove(requestId)
		canonical, err := writeForkChoiceHashes(forkChoice, s, tx, cfg)
		if err != nil {
			log.Warn(fmt.Sprintf("[%s] Fork choice err", s.LogPrefix()), "err", err)
			return nil, err
		}
		if canonical {
			cfg.hd.SetPendingPayloadHash(headerHash)
			return nil, nil
		} else {
			return &engineapi.PayloadStatus{
				CriticalError: &privateapi.InvalidForkchoiceStateErr,
			}, nil
		}
	}

	cfg.hd.UpdateTopSeenHeightPoS(headerNumber)
	forkingPoint, err := forkingPoint(ctx, tx, headerInserter, cfg.blockReader, header)
	if err != nil {
		return nil, err
	}

	log.Info(fmt.Sprintf("[%s] Fork choice re-org", s.LogPrefix()), "headerNumber", headerNumber, "forkingPoint", forkingPoint)

	if requestStatus == engineapi.New {
		if headerNumber-forkingPoint <= ShortPoSReorgThresholdBlocks {
			// TODO(yperbasis): what if some bodies are missing and we have to download them?
			cfg.hd.SetPendingPayloadHash(headerHash)
		} else {
			cfg.hd.PayloadStatusCh <- engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}
		}
	}

	u.UnwindTo(forkingPoint, common.Hash{})

	cfg.hd.SetUnsettledForkChoice(forkChoice, headerNumber)

	return nil, nil
}

func finishHandlingForkChoice(
	forkChoice *engineapi.ForkChoiceMessage,
	headHeight uint64,
	s *StageState,
	tx kv.RwTx,
	cfg HeadersCfg,
	useExternalTx bool,
) error {
	log.Info(fmt.Sprintf("[%s] Unsettled forkchoice after unwind", s.LogPrefix()), "height", headHeight, "forkchoice", forkChoice)

	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()

	if err := fixCanonicalChain(s.LogPrefix(), logEvery, headHeight, forkChoice.HeadBlockHash, tx, cfg.blockReader); err != nil {
		return err
	}

	if err := rawdb.WriteHeadHeaderHash(tx, forkChoice.HeadBlockHash); err != nil {
		return err
	}

	canonical, err := writeForkChoiceHashes(forkChoice, s, tx, cfg)
	if err != nil {
		return err
	}

	if err := s.Update(tx, headHeight); err != nil {
		return err
	}

	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}

	if !canonical {
		if cfg.hd.GetPendingPayloadHash() != (common.Hash{}) {
			cfg.hd.PayloadStatusCh <- engineapi.PayloadStatus{
				CriticalError: &privateapi.InvalidForkchoiceStateErr,
			}
		}
		cfg.hd.ClearPendingPayloadHash()
	}

	cfg.hd.ClearUnsettledForkChoice()
	return nil
}

func handleNewPayload(
	block *types.Block,
	requestStatus engineapi.RequestStatus,
	requestId int,
	s *StageState,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	test bool,
	headerInserter *headerdownload.HeaderInserter,
) (*engineapi.PayloadStatus, error) {
	header := block.Header()
	headerNumber := header.Number.Uint64()
	headerHash := block.Hash()

	log.Debug(fmt.Sprintf("[%s] Handling new payload", s.LogPrefix()), "height", headerNumber, "hash", headerHash)
	cfg.hd.UpdateTopSeenHeightPoS(headerNumber)

	parent, err := cfg.blockReader.HeaderByHash(ctx, tx, header.ParentHash)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		log.Info(fmt.Sprintf("[%s] New payload: need to download parent", s.LogPrefix()), "height", headerNumber, "hash", headerHash, "parentHash", header.ParentHash)
		cfg.hd.BeaconRequestList.Remove(requestId)
		if test {
			return &engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}, nil
		}
		if !schedulePoSDownload(requestId, header.ParentHash, headerNumber-1, headerHash /* downloaderTip */, s, cfg) {
			return &engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}, nil
		}
		currentHeadNumber := rawdb.ReadCurrentBlockNumber(tx)
		if currentHeadNumber != nil && math.AbsoluteDifference(*currentHeadNumber, headerNumber) < 32 {
			// We try waiting until we finish downloading the PoS blocks if the distance from the head is enough,
			// so that we will perform full validation.
			success := false
			for i := 0; i < 10; i++ {
				time.Sleep(10 * time.Millisecond)
				if cfg.hd.PosStatus() == headerdownload.Synced {
					success = true
					break
				}
			}
			if success {
				// If we downloaded the headers in time, then save them and proceed with the new header
				verifyAndSaveDownloadedPoSHeaders(tx, cfg, headerInserter)
			} else {
				return &engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}, nil
			}
		} else {
			return &engineapi.PayloadStatus{Status: remote.EngineStatus_SYNCING}, nil
		}
	}

	cfg.hd.BeaconRequestList.Remove(requestId)

	log.Debug(fmt.Sprintf("[%s] New payload begin verification", s.LogPrefix()))
	response, success, err := verifyAndSaveNewPoSHeader(requestStatus, s, ctx, tx, cfg, block, headerInserter)
	log.Debug(fmt.Sprintf("[%s] New payload verification ended", s.LogPrefix()), "success", success, "err", err)
	if err != nil || !success {
		return response, err
	}

	if cfg.bodyDownload != nil {
		cfg.bodyDownload.AddToPrefetch(block)
	}

	return response, nil
}

func verifyAndSaveNewPoSHeader(
	requestStatus engineapi.RequestStatus,
	s *StageState,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	block *types.Block,
	headerInserter *headerdownload.HeaderInserter,
) (response *engineapi.PayloadStatus, success bool, err error) {
	header := block.Header()
	headerNumber := header.Number.Uint64()
	headerHash := block.Hash()

	bad, lastValidHash := cfg.hd.IsBadHeaderPoS(headerHash)
	if bad {
		return &engineapi.PayloadStatus{Status: remote.EngineStatus_INVALID, LatestValidHash: lastValidHash}, false, nil
	}

	if verificationErr := cfg.hd.VerifyHeader(header); verificationErr != nil {
		log.Warn("Verification failed for header", "hash", headerHash, "height", headerNumber, "err", verificationErr)
		cfg.hd.ReportBadHeaderPoS(headerHash, header.ParentHash)
		return &engineapi.PayloadStatus{
			Status:          remote.EngineStatus_INVALID,
			LatestValidHash: header.ParentHash,
			ValidationError: verificationErr,
		}, false, nil
	}

	currentHeadHash := rawdb.ReadHeadHeaderHash(tx)

	forkingPoint, err := forkingPoint(ctx, tx, headerInserter, cfg.blockReader, header)
	if err != nil {
		return nil, false, err
	}
	forkingHash, err := cfg.blockReader.CanonicalHash(ctx, tx, forkingPoint)

	canExtendCanonical := forkingHash == currentHeadHash

	if cfg.memoryOverlay {
		extendingHash := cfg.forkValidator.ExtendingForkHeadHash()
		extendCanonical := (extendingHash == common.Hash{} && header.ParentHash == currentHeadHash) || extendingHash == header.ParentHash
		status, latestValidHash, validationError, criticalError := cfg.forkValidator.ValidatePayload(tx, header, block.RawBody(), extendCanonical)
		if criticalError != nil {
			return nil, false, criticalError
		}
		success = validationError == nil
		if !success {
			log.Warn("Validation failed for header", "hash", headerHash, "height", headerNumber, "err", validationError)
			cfg.hd.ReportBadHeaderPoS(headerHash, latestValidHash)
		} else if err := headerInserter.FeedHeaderPoS(tx, header, headerHash); err != nil {
			return nil, false, err
		}
		return &engineapi.PayloadStatus{
			Status:          status,
			LatestValidHash: latestValidHash,
			ValidationError: validationError,
		}, success, nil
	}

	if err := headerInserter.FeedHeaderPoS(tx, header, headerHash); err != nil {
		return nil, false, err
	}

	if !canExtendCanonical {
		log.Info("Side chain", "parentHash", header.ParentHash, "currentHead", currentHeadHash)
		return &engineapi.PayloadStatus{Status: remote.EngineStatus_ACCEPTED}, true, nil
	}

	// OK, we're on the canonical chain
	if requestStatus == engineapi.New {
		cfg.hd.SetPendingPayloadHash(headerHash)
	}

	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()

	// Extend canonical chain by the new header
	err = fixCanonicalChain(s.LogPrefix(), logEvery, headerInserter.GetHighest(), headerInserter.GetHighestHash(), tx, cfg.blockReader)
	if err != nil {
		return nil, false, err
	}

	err = rawdb.WriteHeadHeaderHash(tx, headerHash)
	if err != nil {
		return nil, false, err
	}

	err = s.Update(tx, headerNumber)
	if err != nil {
		return nil, false, err
	}

	return nil, true, nil
}

func schedulePoSDownload(
	requestId int,
	hashToDownload common.Hash,
	heightToDownload uint64,
	downloaderTip common.Hash,
	s *StageState,
	cfg HeadersCfg,
) bool {
	cfg.hd.BeaconRequestList.SetStatus(requestId, engineapi.DataWasMissing)

	if cfg.hd.PosStatus() != headerdownload.Idle {
		log.Debug(fmt.Sprintf("[%s] Postponing PoS download since another one is in progress", s.LogPrefix()), "height", heightToDownload, "hash", hashToDownload)
		return false
	}

	log.Info(fmt.Sprintf("[%s] Downloading PoS headers...", s.LogPrefix()), "height", heightToDownload, "hash", hashToDownload, "requestId", requestId)

	cfg.hd.SetRequestId(requestId)
	cfg.hd.SetPoSDownloaderTip(downloaderTip)
	cfg.hd.SetHeaderToDownloadPoS(hashToDownload, heightToDownload)
	cfg.hd.SetPOSSync(true) // This needs to be called after SetHeaderToDownloadPOS because SetHeaderToDownloadPOS sets `posAnchor` member field which is used by ProcessHeadersPOS

	//nolint
	headerCollector := etl.NewCollector(s.LogPrefix(), cfg.tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
	// headerCollector is closed in verifyAndSaveDownloadedPoSHeaders, thus nolint

	cfg.hd.SetHeadersCollector(headerCollector)

	cfg.hd.SetPosStatus(headerdownload.Syncing)

	return true
}

func verifyAndSaveDownloadedPoSHeaders(tx kv.RwTx, cfg HeadersCfg, headerInserter *headerdownload.HeaderInserter) {
	var lastValidHash common.Hash
	var badChainError error
	var foundPow bool

	headerLoadFunc := func(key, value []byte, _ etl.CurrentTableReader, _ etl.LoadNextFunc) error {
		var h types.Header
		if err := rlp.DecodeBytes(value, &h); err != nil {
			return err
		}
		if badChainError != nil {
			cfg.hd.ReportBadHeaderPoS(h.Hash(), lastValidHash)
			return nil
		}
		lastValidHash = h.ParentHash
		if err := cfg.hd.VerifyHeader(&h); err != nil {
			log.Warn("Verification failed for header", "hash", h.Hash(), "height", h.Number.Uint64(), "err", err)
			badChainError = err
			cfg.hd.ReportBadHeaderPoS(h.Hash(), lastValidHash)
			return nil
		}
		// If we are in PoW range then block validation is not required anymore.
		if foundPow {
			return headerInserter.FeedHeaderPoS(tx, &h, h.Hash())
		}

		foundPow = h.Difficulty.Cmp(common.Big0) != 0
		if foundPow {
			return headerInserter.FeedHeaderPoS(tx, &h, h.Hash())
		}
		// Validate state if possible (bodies will be retrieved through body download)
		_, _, validationError, criticalError := cfg.forkValidator.ValidatePayload(tx, &h, nil, false)
		if criticalError != nil {
			return criticalError
		}
		if validationError != nil {
			badChainError = validationError
			cfg.hd.ReportBadHeaderPoS(h.Hash(), lastValidHash)
			return nil
		}

		return headerInserter.FeedHeaderPoS(tx, &h, h.Hash())
	}

	err := cfg.hd.HeadersCollector().Load(tx, kv.Headers, headerLoadFunc, etl.TransformArgs{
		LogDetailsLoad: func(k, v []byte) (additionalLogArguments []interface{}) {
			return []interface{}{"block", binary.BigEndian.Uint64(k)}
		},
	})

	if err != nil || badChainError != nil {
		if err == nil {
			err = badChainError
		}
		log.Warn("Removing beacon request due to", "err", err, "requestId", cfg.hd.RequestId())
		cfg.hd.BeaconRequestList.Remove(cfg.hd.RequestId())
		cfg.hd.ReportBadHeaderPoS(cfg.hd.PoSDownloaderTip(), lastValidHash)
	} else {
		log.Info("PoS headers verified and saved", "requestId", cfg.hd.RequestId(), "fork head", lastValidHash)
	}

	cfg.hd.HeadersCollector().Close()
	cfg.hd.SetHeadersCollector(nil)
	cfg.hd.SetPosStatus(headerdownload.Idle)
}

func forkingPoint(
	ctx context.Context,
	tx kv.RwTx,
	headerInserter *headerdownload.HeaderInserter,
	headerReader services.HeaderReader,
	header *types.Header,
) (uint64, error) {
	headerNumber := header.Number.Uint64()
	if headerNumber == 0 {
		return 0, nil
	}
	parent, err := headerReader.Header(ctx, tx, header.ParentHash, headerNumber-1)
	if err != nil {
		return 0, err
	}
	return headerInserter.ForkingPoint(tx, header, parent)
}

func handleInterrupt(interrupt engineapi.Interrupt, cfg HeadersCfg, tx kv.RwTx, headerInserter *headerdownload.HeaderInserter, useExternalTx bool) (bool, error) {
	if interrupt != engineapi.None {
		if interrupt == engineapi.Stopping {
			close(cfg.hd.ShutdownCh)
			return false, fmt.Errorf("server is stopping")
		}
		if interrupt == engineapi.Synced && cfg.hd.HeadersCollector() != nil {
			verifyAndSaveDownloadedPoSHeaders(tx, cfg, headerInserter)
		}
		if !useExternalTx {
			return true, tx.Commit()
		}
		return true, nil
	}
	return false, nil
}

// HeadersPOW progresses Headers stage for Proof-of-Work headers
func HeadersPOW(
	s *StageState,
	u Unwinder,
	ctx context.Context,
	tx kv.RwTx,
	cfg HeadersCfg,
	initialCycle bool,
	test bool, // Set to true in tests, allows the stage to fail rather than wait indefinitely
	useExternalTx bool,
) error {
	var headerProgress uint64
	var err error

	if err = cfg.hd.ReadProgressFromDb(tx); err != nil {
		return err
	}
	cfg.hd.SetPOSSync(false)
	cfg.hd.SetFetchingNew(true)
	defer cfg.hd.SetFetchingNew(false)
	headerProgress = cfg.hd.Progress()
	logPrefix := s.LogPrefix()
	// Check if this is called straight after the unwinds, which means we need to create new canonical markings
	hash, err := rawdb.ReadCanonicalHash(tx, headerProgress)
	if err != nil {
		return err
	}
	logEvery := time.NewTicker(logInterval)
	defer logEvery.Stop()
	if hash == (common.Hash{}) {
		headHash := rawdb.ReadHeadHeaderHash(tx)
		if err = fixCanonicalChain(logPrefix, logEvery, headerProgress, headHash, tx, cfg.blockReader); err != nil {
			return err
		}
		if !useExternalTx {
			if err = tx.Commit(); err != nil {
				return err
			}
		}
		return nil
	}

	// Allow other stages to run 1 cycle if no network available
	if initialCycle && cfg.noP2PDiscovery {
		return nil
	}

	log.Info(fmt.Sprintf("[%s] Waiting for headers...", logPrefix), "from", headerProgress)

	localTd, err := rawdb.ReadTd(tx, hash, headerProgress)
	if err != nil {
		return err
	}
	if localTd == nil {
		return fmt.Errorf("localTD is nil: %d, %x", headerProgress, hash)
	}
	headerInserter := headerdownload.NewHeaderInserter(logPrefix, localTd, headerProgress, cfg.blockReader)
	cfg.hd.SetHeaderReader(&chainReader{config: &cfg.chainConfig, tx: tx, blockReader: cfg.blockReader})

	var sentToPeer bool
	stopped := false
	prevProgress := headerProgress
	var noProgressCounter int
	var wasProgress bool
	var lastSkeletonTime time.Time
Loop:
	for !stopped {

		transitionedToPoS, err := rawdb.Transitioned(tx, headerProgress, cfg.chainConfig.TerminalTotalDifficulty)
		if err != nil {
			return err
		}
		if transitionedToPoS {
			if err := s.Update(tx, headerProgress); err != nil {
				return err
			}
			break
		}

		currentTime := time.Now()
		req, penalties := cfg.hd.RequestMoreHeaders(currentTime)
		if req != nil {
			_, sentToPeer = cfg.headerReqSend(ctx, req)
			if sentToPeer {
				cfg.hd.UpdateStats(req, false /* skeleton */)
			}
			// Regardless of whether request was actually sent to a peer, we update retry time to be 5 seconds in the future
			cfg.hd.UpdateRetryTime(req, currentTime, 5*time.Second /* timeout */)
		}
		if len(penalties) > 0 {
			cfg.penalize(ctx, penalties)
		}
		maxRequests := 64 // Limit number of requests sent per round to let some headers to be inserted into the database
		for req != nil && sentToPeer && maxRequests > 0 {
			req, penalties = cfg.hd.RequestMoreHeaders(currentTime)
			if req != nil {
				_, sentToPeer = cfg.headerReqSend(ctx, req)
				if sentToPeer {
					cfg.hd.UpdateStats(req, false /* skeleton */)
				}
				// Regardless of whether request was actually sent to a peer, we update retry time to be 5 seconds in the future
				cfg.hd.UpdateRetryTime(req, currentTime, 5*time.Second /* timeout */)
			}
			if len(penalties) > 0 {
				cfg.penalize(ctx, penalties)
			}
			maxRequests--
		}

		// Send skeleton request if required
		if time.Since(lastSkeletonTime) > 1*time.Second {
			req = cfg.hd.RequestSkeleton()
			if req != nil {
				_, sentToPeer = cfg.headerReqSend(ctx, req)
				if sentToPeer {
					cfg.hd.UpdateStats(req, true /* skeleton */)
					lastSkeletonTime = time.Now()
				}
			}
		}
		// Load headers into the database
		var inSync bool
		if inSync, err = cfg.hd.InsertHeaders(headerInserter.NewFeedHeaderFunc(tx, cfg.blockReader), cfg.chainConfig.TerminalTotalDifficulty, logPrefix, logEvery.C); err != nil {
			return err
		}

		if test {
			announces := cfg.hd.GrabAnnounces()
			if len(announces) > 0 {
				cfg.announceNewHashes(ctx, announces)
			}
		}

		if headerInserter.BestHeaderChanged() { // We do not break unless there best header changed
			noProgressCounter = 0
			wasProgress = true
			if !initialCycle {
				// if this is not an initial cycle, we need to react quickly when new headers are coming in
				break
			}
			// if this is initial cycle, we want to make sure we insert all known headers (inSync)
			if inSync {
				break
			}
		}
		if test {
			break
		}
		timer := time.NewTimer(1 * time.Second)
		select {
		case <-ctx.Done():
			stopped = true
		case <-logEvery.C:
			progress := cfg.hd.Progress()
			logProgressHeaders(logPrefix, prevProgress, progress)
			stats := cfg.hd.ExtractStats()
			if prevProgress == progress {
				noProgressCounter++
				if noProgressCounter >= 5 {
					log.Info("Req/resp stats", "req", stats.Requests, "reqMin", stats.ReqMinBlock, "reqMax", stats.ReqMaxBlock,
						"skel", stats.SkeletonRequests, "skelMin", stats.SkeletonReqMinBlock, "skelMax", stats.SkeletonReqMaxBlock,
						"resp", stats.Responses, "respMin", stats.RespMinBlock, "respMax", stats.RespMaxBlock, "dups", stats.Duplicates)
					cfg.hd.LogAnchorState()
					if wasProgress {
						log.Warn("Looks like chain is not progressing, moving to the next stage")
						break Loop
					}
				}
			}
			prevProgress = progress
		case <-timer.C:
			log.Trace("RequestQueueTime (header) ticked")
		case <-cfg.hd.DeliveryNotify:
			log.Trace("headerLoop woken up by the incoming request")
		}
		timer.Stop()
	}
	if headerInserter.Unwind() {
		u.UnwindTo(headerInserter.UnwindPoint(), common.Hash{})
	}
	if headerInserter.GetHighest() != 0 {
		if !headerInserter.Unwind() {
			if err := fixCanonicalChain(logPrefix, logEvery, headerInserter.GetHighest(), headerInserter.GetHighestHash(), tx, cfg.blockReader); err != nil {
				return fmt.Errorf("fix canonical chain: %w", err)
			}
		}
		if err = rawdb.WriteHeadHeaderHash(tx, headerInserter.GetHighestHash()); err != nil {
			return fmt.Errorf("[%s] marking head header hash as %x: %w", logPrefix, headerInserter.GetHighestHash(), err)
		}
		if err = s.Update(tx, headerInserter.GetHighest()); err != nil {
			return fmt.Errorf("[%s] saving Headers progress: %w", logPrefix, err)
		}
	}
	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if stopped {
		return libcommon.ErrStopped
	}
	// We do not print the following line if the stage was interrupted
	log.Info(fmt.Sprintf("[%s] Processed", logPrefix), "highest inserted", headerInserter.GetHighest(), "age", common.PrettyAge(time.Unix(int64(headerInserter.GetHighestTimestamp()), 0)))

	return nil
}

func fixCanonicalChain(logPrefix string, logEvery *time.Ticker, height uint64, hash common.Hash, tx kv.StatelessRwTx, headerReader services.FullBlockReader) error {
	if height == 0 {
		return nil
	}
	ancestorHash := hash
	ancestorHeight := height

	var ch common.Hash
	var err error
	for ch, err = headerReader.CanonicalHash(context.Background(), tx, ancestorHeight); err == nil && ch != ancestorHash; ch, err = headerReader.CanonicalHash(context.Background(), tx, ancestorHeight) {
		if err = rawdb.WriteCanonicalHash(tx, ancestorHash, ancestorHeight); err != nil {
			return fmt.Errorf("marking canonical header %d %x: %w", ancestorHeight, ancestorHash, err)
		}

		ancestor, err := headerReader.Header(context.Background(), tx, ancestorHash, ancestorHeight)
		if err != nil {
			return err
		}
		if ancestor == nil {
			return fmt.Errorf("ancestor is nil. height %d, hash %x", ancestorHeight, ancestorHash)
		}

		select {
		case <-logEvery.C:
			log.Info(fmt.Sprintf("[%s] write canonical markers", logPrefix), "ancestor", ancestorHeight, "hash", ancestorHash)
		default:
		}
		ancestorHash = ancestor.ParentHash
		ancestorHeight--
	}
	if err != nil {
		return fmt.Errorf("reading canonical hash for %d: %w", ancestorHeight, err)
	}

	return nil
}

func HeadersUnwind(u *UnwindState, s *StageState, tx kv.RwTx, cfg HeadersCfg, test bool) (err error) {
	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err = cfg.db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	// Delete canonical hashes that are being unwound
	badBlock := u.BadBlock != (common.Hash{})
	if badBlock {
		cfg.hd.ReportBadHeader(u.BadBlock)
		// Mark all descendants of bad block as bad too
		headerCursor, cErr := tx.Cursor(kv.Headers)
		if cErr != nil {
			return cErr
		}
		defer headerCursor.Close()
		var k, v []byte
		for k, v, err = headerCursor.Seek(dbutils.EncodeBlockNumber(u.UnwindPoint + 1)); err == nil && k != nil; k, v, err = headerCursor.Next() {
			var h types.Header
			if err = rlp.DecodeBytes(v, &h); err != nil {
				return err
			}
			if cfg.hd.IsBadHeader(h.ParentHash) {
				cfg.hd.ReportBadHeader(h.Hash())
			}
		}
		if err != nil {
			return fmt.Errorf("iterate over headers to mark bad headers: %w", err)
		}
	}
	if err := rawdb.TruncateCanonicalHash(tx, u.UnwindPoint+1, false /* deleteHeaders */); err != nil {
		return err
	}
	if badBlock {
		var maxTd big.Int
		var maxHash common.Hash
		var maxNum uint64 = 0

		if test { // If we are not in the test, we can do searching for the heaviest chain in the next cycle
			// Find header with biggest TD
			tdCursor, cErr := tx.Cursor(kv.HeaderTD)
			if cErr != nil {
				return cErr
			}
			defer tdCursor.Close()
			var k, v []byte
			k, v, err = tdCursor.Last()
			if err != nil {
				return err
			}
			for ; err == nil && k != nil; k, v, err = tdCursor.Prev() {
				if len(k) != 40 {
					return fmt.Errorf("key in TD table has to be 40 bytes long: %x", k)
				}
				var hash common.Hash
				copy(hash[:], k[8:])
				if cfg.hd.IsBadHeader(hash) {
					continue
				}
				var td big.Int
				if err = rlp.DecodeBytes(v, &td); err != nil {
					return err
				}
				if td.Cmp(&maxTd) > 0 {
					maxTd.Set(&td)
					copy(maxHash[:], k[8:])
					maxNum = binary.BigEndian.Uint64(k[:8])
				}
			}
			if err != nil {
				return err
			}
		}
		/* TODO(yperbasis): Is it safe?
		if err := rawdb.TruncateTd(tx, u.UnwindPoint+1); err != nil {
			return err
		}
		*/
		if maxNum == 0 {
			maxNum = u.UnwindPoint
			if maxHash, err = rawdb.ReadCanonicalHash(tx, maxNum); err != nil {
				return err
			}
		}
		if err = rawdb.WriteHeadHeaderHash(tx, maxHash); err != nil {
			return err
		}
		if err = u.Done(tx); err != nil {
			return err
		}
		if err = s.Update(tx, maxNum); err != nil {
			return err
		}
	}
	if !useExternalTx {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func logProgressHeaders(logPrefix string, prev, now uint64) uint64 {
	speed := float64(now-prev) / float64(logInterval/time.Second)
	if speed == 0 {
		// Don't log "Wrote block ..." unless we're actually writing something
		return now
	}

	var m runtime.MemStats
	libcommon.ReadMemStats(&m)
	log.Info(fmt.Sprintf("[%s] Wrote block headers", logPrefix),
		"number", now,
		"blk/second", speed,
		"alloc", libcommon.ByteCount(m.Alloc),
		"sys", libcommon.ByteCount(m.Sys))

	return now
}

type chainReader struct {
	config      *params.ChainConfig
	tx          kv.RwTx
	blockReader services.FullBlockReader
}

func (cr chainReader) Config() *params.ChainConfig  { return cr.config }
func (cr chainReader) CurrentHeader() *types.Header { panic("") }
func (cr chainReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.Header(context.Background(), cr.tx, hash, number)
		return h
	}
	return rawdb.ReadHeader(cr.tx, hash, number)
}
func (cr chainReader) GetHeaderByNumber(number uint64) *types.Header {
	if cr.blockReader != nil {
		h, _ := cr.blockReader.HeaderByNumber(context.Background(), cr.tx, number)
		return h
	}
	return rawdb.ReadHeaderByNumber(cr.tx, number)

}
func (cr chainReader) GetHeaderByHash(hash common.Hash) *types.Header {
	if cr.blockReader != nil {
		number := rawdb.ReadHeaderNumber(cr.tx, hash)
		if number == nil {
			return nil
		}
		return cr.GetHeader(hash, *number)
	}
	h, _ := rawdb.ReadHeaderByHash(cr.tx, hash)
	return h
}
func (cr chainReader) GetTd(hash common.Hash, number uint64) *big.Int {
	td, err := rawdb.ReadTd(cr.tx, hash, number)
	if err != nil {
		log.Error("ReadTd failed", "err", err)
		return nil
	}
	return td
}

type epochReader struct {
	tx kv.RwTx
}

func (cr epochReader) GetEpoch(hash common.Hash, number uint64) ([]byte, error) {
	return rawdb.ReadEpoch(cr.tx, number, hash)
}
func (cr epochReader) PutEpoch(hash common.Hash, number uint64, proof []byte) error {
	return rawdb.WriteEpoch(cr.tx, number, hash, proof)
}
func (cr epochReader) GetPendingEpoch(hash common.Hash, number uint64) ([]byte, error) {
	return rawdb.ReadPendingEpoch(cr.tx, number, hash)
}
func (cr epochReader) PutPendingEpoch(hash common.Hash, number uint64, proof []byte) error {
	return rawdb.WritePendingEpoch(cr.tx, number, hash, proof)
}
func (cr epochReader) FindBeforeOrEqualNumber(number uint64) (blockNum uint64, blockHash common.Hash, transitionProof []byte, err error) {
	return rawdb.FindEpochBeforeOrEqualNumber(cr.tx, number)
}

func HeadersPrune(p *PruneState, tx kv.RwTx, cfg HeadersCfg, ctx context.Context) (err error) {
	useExternalTx := tx != nil
	if !useExternalTx {
		tx, err = cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if !useExternalTx {
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
