package aggsender

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	zkevm "github.com/0xPolygon/cdk"
	"github.com/0xPolygon/cdk/agglayer"
	"github.com/0xPolygon/cdk/aggsender"
	"github.com/0xPolygon/cdk/aggsender/db"
	"github.com/0xPolygon/cdk/aggsender/types"
	"github.com/0xPolygon/cdk/bridgesync"
	cdkdb "github.com/0xPolygon/cdk/db"
	"github.com/0xPolygon/cdk/l1infotreesync"
	"github.com/0xPolygon/cdk/log"
	"github.com/0xPolygon/cdk/tree"
	treeTypes "github.com/0xPolygon/cdk/tree/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/russross/meddler"
)

const signatureSize = 65

var (
	errNoBridgesAndClaims   = errors.New("no bridges and claims to build certificate")
	errInvalidSignatureSize = errors.New("invalid signature size")

	zeroLER = common.HexToHash("0x27ae5ba08d7291c96c8cbddcc148bf48a6d68c7974b94356f53754ef6171d757")
)

// AggSender is a component that will send certificates to the aggLayer
type AggSender struct {
	log types.Logger

	l2Syncer         types.L2BridgeSyncer
	l1infoTreeSyncer types.L1InfoTreeSyncer
	epochNotifier    types.EpochNotifier

	storage        db.AggSenderStorage
	aggLayerClient agglayer.AgglayerClientInterface

	cfg aggsender.Config

	sequencerKey *ecdsa.PrivateKey
	Tree         *tree.AppendOnlyTree

	BridgeDatabase *sql.DB

	status types.AggsenderStatus
}

// New returns a new AggSender
func New(
	ctx context.Context,
	logger *log.Logger,
	cfg aggsender.Config,
	aggLayerClient agglayer.AgglayerClientInterface,
	l1InfoTreeSyncer *l1infotreesync.L1InfoTreeSync,
	l2Syncer types.L2BridgeSyncer,
	epochNotifier types.EpochNotifier,
	sequencerPrivateKey *ecdsa.PrivateKey,
	bridgeDB string,
) (*AggSender, error) {
	storageConfig := db.AggSenderSQLStorageConfig{
		DBPath:                  cfg.StoragePath,
		KeepCertificatesHistory: cfg.KeepCertificatesHistory,
	}
	storage, err := db.NewAggSenderSQLStorage(logger, storageConfig)
	if err != nil {
		return nil, err
	}

	logger.Infof("Aggsender Config: %s.", cfg.String())

	bridgeDatabase, tree, err := ConnectTree(bridgeDB)
	if err != nil {
		return nil, err
	}
	return &AggSender{
		cfg:              cfg,
		log:              logger,
		storage:          storage,
		l2Syncer:         l2Syncer,
		aggLayerClient:   aggLayerClient,
		l1infoTreeSyncer: l1InfoTreeSyncer,
		sequencerKey:     sequencerPrivateKey,
		epochNotifier:    epochNotifier,
		Tree:             tree,
		BridgeDatabase:   bridgeDatabase,
		status:           types.AggsenderStatus{Status: types.StatusNone},
	}, nil
}

func (a *AggSender) Info() types.AggsenderInfo {
	res := types.AggsenderInfo{
		AggsenderStatus:          a.status,
		Version:                  zkevm.GetVersion(),
		EpochNotifierDescription: a.epochNotifier.String(),
		NetworkID:                a.l2Syncer.OriginNetwork(),
	}
	return res
}

// Start starts the AggSender
func (a *AggSender) Start(ctx context.Context, emptyCert, addFakeBridge, storeCertificate, singleCert bool) {
	a.log.Info("AggSender started")
	a.status.Start(time.Now().UTC())
	a.checkInitialStatus(ctx)
	a.sendCertificates(ctx, emptyCert, addFakeBridge, storeCertificate, singleCert)
}

// checkInitialStatus check local status vs agglayer status
func (a *AggSender) checkInitialStatus(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.DelayBeetweenRetries.Duration)
	defer ticker.Stop()
	a.status.Status = types.StatusCheckingInitialStage
	for {
		err := a.checkLastCertificateFromAgglayer(ctx)
		a.status.SetLastError(err)
		if err != nil {
			a.log.Errorf("error checking initial status: %v, retrying in %s", err, a.cfg.DelayBeetweenRetries.String())
		} else {
			a.log.Info("Initial status checked successfully")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sendCertificates sends certificates to the aggLayer
func (a *AggSender) sendCertificates(ctx context.Context, emptyCert, addFakeBridge, storeCertificate, singleCert bool) {
	ticker := time.NewTicker(time.Second)
	a.status.Status = types.StatusCertificateStage
	for {
		select {
		case <-ticker.C:
			thereArePendingCerts := a.checkPendingCertificatesStatus(ctx)
			if !thereArePendingCerts {
				_, err := a.sendCertificate(ctx, emptyCert, addFakeBridge, storeCertificate, singleCert)
				a.status.SetLastError(err)
				if err != nil {
					a.log.Error(err)
				}
			} else {
				log.Infof("Skipping because there are pending certificates")
		}
		case <-ctx.Done():
			a.log.Info("AggSender stopped")
			return
		}
	}
}

// sendCertificate sends certificate for a network
func (a *AggSender) sendCertificate(ctx context.Context, emptyCert, addFakeBridge, storeCertificate, singleCert bool) (*agglayer.SignedCertificate, error) {
	a.log.Infof("trying to send a new certificate...")

	lastL2BlockSynced, err := a.l2Syncer.GetLastProcessedBlock(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting last processed block from l2: %w", err)
	}

	lastSentCertificateInfo, err := a.storage.GetLastSentCertificate()
	if err != nil {
		return nil, err
	}

	previousToBlock, retryCount := getLastSentBlockAndRetryCount(lastSentCertificateInfo)

	if previousToBlock >= lastL2BlockSynced {
		a.log.Infof("no new blocks to send a certificate, last certificate block: %d, last L2 block: %d",
			previousToBlock, lastL2BlockSynced)
		return nil, nil
	}

	fromBlock := previousToBlock + 1
	toBlock := lastL2BlockSynced

	bridges, err := a.l2Syncer.GetBridgesPublished(ctx, fromBlock, toBlock)
	if err != nil {
		return nil, fmt.Errorf("error getting bridges: %w", err)
	}

	if len(bridges) == 0 {
		a.log.Infof("no bridges consumed, no need to send a certificate from block: %d to block: %d", fromBlock, toBlock)
		return nil, nil
	}

	claims, err := a.l2Syncer.GetClaims(ctx, fromBlock, toBlock)
	if err != nil {
		return nil, fmt.Errorf("error getting claims: %w", err)
	}
	certificateParams := &types.CertificateBuildParams{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Bridges:   bridges,
		Claims:    claims,
		CreatedAt: uint32(time.Now().UTC().Unix()),
	}

	certificateParams, err = a.limitCertSize(certificateParams)
	if err != nil {
		return nil, fmt.Errorf("error limitCertSize: %w", err)
	}
	a.log.Infof("building certificate for %s estimatedSize=%d", certificateParams.String(), certificateParams.EstimatedSize())

	certificate, err := a.buildCertificate(ctx, certificateParams, lastSentCertificateInfo, addFakeBridge)
	if err != nil {
		return nil, fmt.Errorf("error building certificate: %w", err)
	}

	if emptyCert {
		log.Info("Removing bridges and claims from certificate to send the empty certificate")
		certificate.BridgeExits = []*agglayer.BridgeExit{}
		certificate.ImportedBridgeExits = []*agglayer.ImportedBridgeExit{}
	}

	signedCertificate, err := a.signCertificate(certificate)
	if err != nil {
		return nil, fmt.Errorf("error signing certificate: %w", err)
	}

	a.saveCertificateToFile(signedCertificate)
	a.log.Infof("certificate ready to be send to AggLayer: %s", signedCertificate.Brief())
	if a.cfg.DryRun {
		a.log.Warn("dry run mode enabled, skipping sending certificate")
		return signedCertificate, nil
	}
	certificateHash, err := a.aggLayerClient.SendCertificate(signedCertificate)
	if err != nil {
		return nil, fmt.Errorf("error sending certificate: %w", err)
	}

	a.log.Debugf("Certificate sent with hash: %s height: %d, cert: %s", certificateHash.String(), signedCertificate.Height, signedCertificate.Brief())

	raw, err := json.Marshal(signedCertificate)
	if err != nil {
		return nil, fmt.Errorf("error marshalling signed certificate. Cert:%s. Err: %w", signedCertificate.Brief(), err)
	}
	log.Debug("certificate sent: ", string(raw))
	if !storeCertificate {
		log.Info("storeCertificate is disabled. Skipping storing the certificate in the database")
		return signedCertificate, nil
	}

	prevLER := common.BytesToHash(certificate.PrevLocalExitRoot[:])
	certInfo := types.CertificateInfo{
		Height:                certificate.Height,
		RetryCount:            retryCount,
		CertificateID:         certificateHash,
		NewLocalExitRoot:      certificate.NewLocalExitRoot,
		PreviousLocalExitRoot: &prevLER,
		FromBlock:             fromBlock,
		ToBlock:               toBlock,
		CreatedAt:             certificateParams.CreatedAt,
		UpdatedAt:             certificateParams.CreatedAt,
		SignedCertificate:     string(raw),
	}
	// TODO: Improve this case, if a cert is not save in the storage, we are going to settle a unknown certificate
	err = a.saveCertificateToStorage(ctx, certInfo, a.cfg.MaxRetriesStoreCertificate)
	if err != nil {
		a.log.Errorf("error saving certificate  to storage. Cert:%s Err: %w", certInfo.String(), err)
		return nil, fmt.Errorf("error saving last sent certificate %s in db: %w", certInfo.String(), err)
	}

	a.log.Infof("certificate: %s sent successfully for range of l2 blocks (from block: %d, to block: %d) cert:%s", certInfo.ID(), fromBlock, toBlock, signedCertificate.Brief())

	if singleCert {
		log.Info("Single certificate mode enabled, exiting")
		os.Exit(0)
	}
	return signedCertificate, nil
}

// saveCertificateToStorage saves the certificate to the storage
// it retries if it fails. if param retries == 0 it retries indefinitely
func (a *AggSender) saveCertificateToStorage(ctx context.Context, cert types.CertificateInfo, maxRetries int) error {
	retries := 1
	err := fmt.Errorf("initial_error")
	for err != nil {
		if err = a.storage.SaveLastSentCertificate(ctx, cert); err != nil {
			// If this happens we can't work as normal, because local DB is outdated, we have to retry
			a.log.Errorf("error saving last sent certificate %s in db: %w", cert.String(), err)
			if retries == maxRetries {
				return fmt.Errorf("error saving last sent certificate %s in db: %w", cert.String(), err)
			} else {
				retries++
				time.Sleep(a.cfg.DelayBeetweenRetries.Duration)
			}
		}
	}
	return nil
}

func (a *AggSender) limitCertSize(fullCert *types.CertificateBuildParams) (*types.CertificateBuildParams, error) {
	currentCert := fullCert
	var previousCert *types.CertificateBuildParams
	var err error
	for {
		if currentCert.NumberOfBridges() == 0 {
			// We can't reduce more the certificate, so this is the minium size
			a.log.Warnf("We reach the minium size of bridge.Certificate size: %d >max size: %d",
				previousCert.EstimatedSize(), a.cfg.MaxCertSize)
			return previousCert, nil
		}

		if a.cfg.MaxCertSize == 0 || currentCert.EstimatedSize() < a.cfg.MaxCertSize {
			return currentCert, nil
		}

		// Minimum size of the certificate
		if currentCert.NumberOfBlocks() <= 1 {
			a.log.Warnf("reach the minium num blocks [%d to %d].Certificate size: %d >max size: %d",
				currentCert.FromBlock, currentCert.ToBlock, currentCert.EstimatedSize(), a.cfg.MaxCertSize)
			return currentCert, nil
		}
		previousCert = currentCert
		currentCert, err = currentCert.Range(currentCert.FromBlock, currentCert.ToBlock-1)
		if err != nil {
			return nil, fmt.Errorf("error reducing certificate: %w", err)
		}
	}
}

// saveCertificate saves the certificate to a tmp file
func (a *AggSender) saveCertificateToFile(signedCertificate *agglayer.SignedCertificate) {
	if signedCertificate == nil || a.cfg.SaveCertificatesToFilesPath == "" {
		return
	}
	fn := fmt.Sprintf("%s/certificate_%04d-%07d.json",
		a.cfg.SaveCertificatesToFilesPath, signedCertificate.Height, time.Now().Unix())
	a.log.Infof("saving certificate to file: %s", fn)
	jsonData, err := json.MarshalIndent(signedCertificate, "", "  ")
	if err != nil {
		a.log.Errorf("error marshalling certificate: %w", err)
	}

	if err = os.WriteFile(fn, jsonData, 0644); err != nil { //nolint:gosec,mnd // we are writing to a tmp file
		a.log.Errorf("error writing certificate to file: %w", err)
	}
}

// getNextHeightAndPreviousLER returns the height and previous LER for the new certificate
func (a *AggSender) getNextHeightAndPreviousLER(
	lastSentCertificateInfo *types.CertificateInfo) (uint64, common.Hash, error) {
	if lastSentCertificateInfo == nil {
		return 0, zeroLER, nil
	}
	if !lastSentCertificateInfo.Status.IsClosed() {
		return 0, zeroLER, fmt.Errorf("last certificate %s is not closed (status: %s)",
			lastSentCertificateInfo.ID(), lastSentCertificateInfo.Status.String())
	}
	if lastSentCertificateInfo.Status.IsSettled() {
		return lastSentCertificateInfo.Height + 1, lastSentCertificateInfo.NewLocalExitRoot, nil
	}

	if lastSentCertificateInfo.Status.IsInError() {
		// We can reuse last one of lastCert?
		if lastSentCertificateInfo.PreviousLocalExitRoot != nil {
			return lastSentCertificateInfo.Height, *lastSentCertificateInfo.PreviousLocalExitRoot, nil
		}
		// Is the first one, so we can set the zeroLER
		if lastSentCertificateInfo.Height == 0 {
			return 0, zeroLER, nil
		}
		// We get previous certificate that must be settled
		a.log.Debugf("last certificate %s is in error, getting previous settled certificate height:%d",
			lastSentCertificateInfo.Height-1)
		lastSettleCert, err := a.storage.GetCertificateByHeight(lastSentCertificateInfo.Height - 1)
		if err != nil {
			return 0, common.Hash{}, fmt.Errorf("error getting last settled certificate: %w", err)
		}
		if lastSettleCert == nil {
			return 0, common.Hash{}, fmt.Errorf("none settled certificate: %w", err)
		}
		if !lastSettleCert.Status.IsSettled() {
			return 0, common.Hash{}, fmt.Errorf("last settled certificate %s is not settled (status: %s)",
				lastSettleCert.ID(), lastSettleCert.Status.String())
		}

		return lastSentCertificateInfo.Height, lastSettleCert.NewLocalExitRoot, nil
	}
	return 0, zeroLER, fmt.Errorf("last certificate %s has an unknown status: %s",
		lastSentCertificateInfo.ID(), lastSentCertificateInfo.Status.String())
}

// buildCertificate builds a certificate from the bridge events
func (a *AggSender) buildCertificate(ctx context.Context, certParams *types.CertificateBuildParams, lastSentCertificateInfo *types.CertificateInfo, addFakeBridge bool) (*agglayer.Certificate, error) {
	if certParams.IsEmpty() {
		return nil, errNoBridgesAndClaims
	}

	bridgeExits := a.getBridgeExits(certParams.Bridges)

	importedBridgeExits, err := a.getImportedBridgeExits(ctx, certParams.Claims)
	if err != nil {
		return nil, fmt.Errorf("error getting imported bridge exits: %w", err)
	}

	var exitRoot treeTypes.Root
	if addFakeBridge {
		log.Info("Adding a fake bridge to the certificate")
		tx, err := cdkdb.NewTx(ctx, a.BridgeDatabase)
		if err != nil {
			return nil, err
		}
		prevDepositCount := certParams.MaxDepositCount()

		fakeBridge := bridgesync.Bridge{
			BlockNum:           0,
			BlockPos:           0,
			LeafType:           0,
			OriginNetwork:      0,
			OriginAddress:      common.Address{},
			DestinationNetwork: 0,
			DestinationAddress: common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"),
			Amount:             big.NewInt(1000),
			Metadata:           []byte{},
			DepositCount:       prevDepositCount + 1,
		}

		err = a.Tree.AddLeaf(tx, fakeBridge.BlockNum, fakeBridge.BlockPos, treeTypes.Leaf{
			Index: fakeBridge.DepositCount,
			Hash:  fakeBridge.Hash(),
		})
		if err != nil {
			log.Warn("error adding the fake leaf. Error: ", err)
			errRollBack := tx.Rollback()
			if errRollBack != nil {
				log.Error("error rolling back the transaction. ErrRollBack: ", errRollBack)
				return nil, errRollBack
			}
			return nil, err
		}
		exitRoot, err = a.GetRootByIndex(ctx, fakeBridge.DepositCount, tx)
		if err != nil {
			log.Error("error getting the root for the fake bridge. Error: ", err)
			errRollBack := tx.Rollback()
			if errRollBack != nil {
				log.Error("error rolling back the transaction. ErrRollBack: ", errRollBack)
				return nil, errRollBack
			}
			return nil, err
		}
		// Rollback to avoid store this fake bridge into the db
		err = tx.Rollback()
		if err != nil {
			log.Error("error rolling back the transaction. Error: ", err)
			return nil, err
		}
		metaData, isMetadataHashed := convertBridgeMetadata(fakeBridge.Metadata, a.cfg.BridgeMetadataAsHash)
		bridgeExits = append(bridgeExits, &agglayer.BridgeExit{
			LeafType: agglayer.LeafType(fakeBridge.LeafType),
			TokenInfo: &agglayer.TokenInfo{
				OriginNetwork:      fakeBridge.OriginNetwork,
				OriginTokenAddress: fakeBridge.OriginAddress,
			},
			DestinationNetwork: fakeBridge.DestinationNetwork,
			DestinationAddress: fakeBridge.DestinationAddress,
			Amount:             fakeBridge.Amount,
			IsMetadataHashed:   isMetadataHashed,
			Metadata:           metaData,
		})
		log.Debugf("ExitRoot got from the fake bridge. Roor: %s, fakeDepositCount: %d", exitRoot.Hash.String(), fakeBridge.DepositCount)
	} else {
		depositCount := certParams.MaxDepositCount()
		exitRoot, err = a.l2Syncer.GetExitRootByIndex(ctx, depositCount)
		if err != nil {
			return nil, fmt.Errorf("error getting exit root by index: %d. Error: %w", depositCount, err)
		}
	}

	height, previousLER, err := a.getNextHeightAndPreviousLER(lastSentCertificateInfo)
	if err != nil {
		return nil, fmt.Errorf("error getting next height and previous LER: %w", err)
	}

	meta := types.NewCertificateMetadata(
		certParams.FromBlock,
		uint32(certParams.ToBlock-certParams.FromBlock),
		certParams.CreatedAt,
	)

	return &agglayer.Certificate{
		NetworkID:           a.l2Syncer.OriginNetwork(),
		PrevLocalExitRoot:   previousLER,
		NewLocalExitRoot:    exitRoot.Hash,
		BridgeExits:         bridgeExits,
		ImportedBridgeExits: importedBridgeExits,
		Height:              height,
		Metadata:            meta.ToHash(),
	}, nil
}

// createCertificateMetadata creates the metadata for the certificate
// it returns: newMetadata + bool if the metadata is hashed or not
func convertBridgeMetadata(metadata []byte, importedBridgeMetadataAsHash bool) ([]byte, bool) {
	var metaData []byte
	var isMetadataHashed bool
	if importedBridgeMetadataAsHash && len(metadata) > 0 {
		metaData = crypto.Keccak256(metadata)
		isMetadataHashed = true
	} else {
		metaData = metadata
		isMetadataHashed = false
	}
	return metaData, isMetadataHashed
}

// convertClaimToImportedBridgeExit converts a claim to an ImportedBridgeExit object
func (a *AggSender) convertClaimToImportedBridgeExit(claim bridgesync.Claim) (*agglayer.ImportedBridgeExit, error) {
	leafType := agglayer.LeafTypeAsset
	if claim.IsMessage {
		leafType = agglayer.LeafTypeMessage
	}
	metaData, isMetadataIsHashed := convertBridgeMetadata(claim.Metadata, a.cfg.BridgeMetadataAsHash)

	bridgeExit := &agglayer.BridgeExit{
		LeafType: leafType,
		TokenInfo: &agglayer.TokenInfo{
			OriginNetwork:      claim.OriginNetwork,
			OriginTokenAddress: claim.OriginAddress,
		},
		DestinationNetwork: claim.DestinationNetwork,
		DestinationAddress: claim.DestinationAddress,
		Amount:             claim.Amount,
		IsMetadataHashed:   isMetadataIsHashed,
		Metadata:           metaData,
	}

	mainnetFlag, rollupIndex, leafIndex, err := bridgesync.DecodeGlobalIndex(claim.GlobalIndex)
	if err != nil {
		return nil, fmt.Errorf("error decoding global index: %w", err)
	}

	return &agglayer.ImportedBridgeExit{
		BridgeExit: bridgeExit,
		GlobalIndex: &agglayer.GlobalIndex{
			MainnetFlag: mainnetFlag,
			RollupIndex: rollupIndex,
			LeafIndex:   leafIndex,
		},
	}, nil
}

// getBridgeExits converts bridges to agglayer.BridgeExit objects
func (a *AggSender) getBridgeExits(bridges []bridgesync.Bridge) []*agglayer.BridgeExit {
	bridgeExits := make([]*agglayer.BridgeExit, 0, len(bridges))

	for _, bridge := range bridges {
		metaData, isMetadataHashed := convertBridgeMetadata(bridge.Metadata, a.cfg.BridgeMetadataAsHash)
		bridgeExits = append(bridgeExits, &agglayer.BridgeExit{
			LeafType: agglayer.LeafType(bridge.LeafType),
			TokenInfo: &agglayer.TokenInfo{
				OriginNetwork:      bridge.OriginNetwork,
				OriginTokenAddress: bridge.OriginAddress,
			},
			DestinationNetwork: bridge.DestinationNetwork,
			DestinationAddress: bridge.DestinationAddress,
			Amount:             bridge.Amount,
			IsMetadataHashed:   isMetadataHashed,
			Metadata:           metaData,
		})
	}

	return bridgeExits
}

// getImportedBridgeExits converts claims to agglayer.ImportedBridgeExit objects and calculates necessary proofs
func (a *AggSender) getImportedBridgeExits(
	ctx context.Context, claims []bridgesync.Claim,
) ([]*agglayer.ImportedBridgeExit, error) {
	if len(claims) == 0 {
		// no claims to convert
		return []*agglayer.ImportedBridgeExit{}, nil
	}

	var (
		greatestL1InfoTreeIndexUsed uint32
		importedBridgeExits         = make([]*agglayer.ImportedBridgeExit, 0, len(claims))
		claimL1Info                 = make([]*l1infotreesync.L1InfoTreeLeaf, 0, len(claims))
	)

	for _, claim := range claims {
		info, err := a.l1infoTreeSyncer.GetInfoByGlobalExitRoot(claim.GlobalExitRoot)
		if err != nil {
			return nil, fmt.Errorf("error getting info by global exit root: %w", err)
		}

		claimL1Info = append(claimL1Info, info)

		if info.L1InfoTreeIndex > greatestL1InfoTreeIndexUsed {
			greatestL1InfoTreeIndexUsed = info.L1InfoTreeIndex
		}
	}

	rootToProve, err := a.l1infoTreeSyncer.GetL1InfoTreeRootByIndex(ctx, greatestL1InfoTreeIndexUsed)
	if err != nil {
		return nil, fmt.Errorf("error getting L1 Info tree root by index: %d. Error: %w", greatestL1InfoTreeIndexUsed, err)
	}

	for i, claim := range claims {
		l1Info := claimL1Info[i]

		a.log.Debugf("claim[%d]: destAddr: %s GER: %s Block: %d Pos: %d GlobalIndex: 0x%x",
			i, claim.DestinationAddress.String(), claim.GlobalExitRoot.String(),
			claim.BlockNum, claim.BlockPos, claim.GlobalIndex)
		ibe, err := a.convertClaimToImportedBridgeExit(claim)
		if err != nil {
			return nil, fmt.Errorf("error converting claim to imported bridge exit: %w", err)
		}

		importedBridgeExits = append(importedBridgeExits, ibe)

		gerToL1Proof, err := a.l1infoTreeSyncer.GetL1InfoTreeMerkleProofFromIndexToRoot(
			ctx, l1Info.L1InfoTreeIndex, rootToProve.Hash,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"error getting L1 Info tree merkle proof for leaf index: %d and root: %s. Error: %w",
				l1Info.L1InfoTreeIndex, rootToProve.Hash, err,
			)
		}

		claim := claims[i]
		if ibe.GlobalIndex.MainnetFlag {
			ibe.ClaimData = &agglayer.ClaimFromMainnnet{
				L1Leaf: &agglayer.L1InfoTreeLeaf{
					L1InfoTreeIndex: l1Info.L1InfoTreeIndex,
					RollupExitRoot:  claim.RollupExitRoot,
					MainnetExitRoot: claim.MainnetExitRoot,
					Inner: &agglayer.L1InfoTreeLeafInner{
						GlobalExitRoot: l1Info.GlobalExitRoot,
						Timestamp:      l1Info.Timestamp,
						BlockHash:      l1Info.PreviousBlockHash,
					},
				},
				ProofLeafMER: &agglayer.MerkleProof{
					Root:  claim.MainnetExitRoot,
					Proof: claim.ProofLocalExitRoot,
				},
				ProofGERToL1Root: &agglayer.MerkleProof{
					Root:  rootToProve.Hash,
					Proof: gerToL1Proof,
				},
			}
		} else {
			ibe.ClaimData = &agglayer.ClaimFromRollup{
				L1Leaf: &agglayer.L1InfoTreeLeaf{
					L1InfoTreeIndex: l1Info.L1InfoTreeIndex,
					RollupExitRoot:  claim.RollupExitRoot,
					MainnetExitRoot: claim.MainnetExitRoot,
					Inner: &agglayer.L1InfoTreeLeafInner{
						GlobalExitRoot: l1Info.GlobalExitRoot,
						Timestamp:      l1Info.Timestamp,
						BlockHash:      l1Info.PreviousBlockHash,
					},
				},
				ProofLeafLER: &agglayer.MerkleProof{
					Root: tree.CalculateRoot(ibe.BridgeExit.Hash(),
						claim.ProofLocalExitRoot, ibe.GlobalIndex.LeafIndex),
					Proof: claim.ProofLocalExitRoot,
				},
				ProofLERToRER: &agglayer.MerkleProof{
					Root:  claim.RollupExitRoot,
					Proof: claim.ProofRollupExitRoot,
				},
				ProofGERToL1Root: &agglayer.MerkleProof{
					Root:  rootToProve.Hash,
					Proof: gerToL1Proof,
				},
			}
		}
	}

	return importedBridgeExits, nil
}

// signCertificate signs a certificate with the sequencer key
func (a *AggSender) signCertificate(certificate *agglayer.Certificate) (*agglayer.SignedCertificate, error) {
	hashToSign := certificate.HashToSign()

	sig, err := crypto.Sign(hashToSign.Bytes(), a.sequencerKey)
	if err != nil {
		return nil, err
	}

	a.log.Infof("Signed certificate. sequencer address: %s. New local exit root: %s Hash signed: %s",
		crypto.PubkeyToAddress(a.sequencerKey.PublicKey).String(),
		common.BytesToHash(certificate.NewLocalExitRoot[:]).String(),
		hashToSign.String(),
	)

	r, s, isOddParity, err := extractSignatureData(sig)
	if err != nil {
		return nil, err
	}

	return &agglayer.SignedCertificate{
		Certificate: certificate,
		Signature: &agglayer.Signature{
			R:         r,
			S:         s,
			OddParity: isOddParity,
		},
	}, nil
}

// checkPendingCertificatesStatus checks the status of pending certificates
// and updates in the storage if it changed on agglayer
// It returns:
// bool -> if there are pending certificates
func (a *AggSender) checkPendingCertificatesStatus(ctx context.Context) bool {
	pendingCertificates, err := a.storage.GetCertificatesByStatus(agglayer.NonSettledStatuses)
	if err != nil {
		a.log.Errorf("error getting pending certificates: %w", err)
		return true
	}

	a.log.Debugf("checkPendingCertificatesStatus num of pendingCertificates: %d", len(pendingCertificates))
	thereArePendingCerts := false

	for _, certificate := range pendingCertificates {
		certificateHeader, err := a.aggLayerClient.GetCertificateHeader(certificate.CertificateID)
		if err != nil {
			a.log.Errorf("error getting certificate header of %s from agglayer: %w",
				certificate.ID(), err)
			return true
		}

		a.log.Debugf("aggLayerClient.GetCertificateHeader status [%s] of certificate %s  elapsed time:%s",
			certificateHeader.Status,
			certificateHeader.ID(),
			certificate.ElapsedTimeSinceCreation())

		if err := a.updateCertificateStatus(ctx, certificate, certificateHeader); err != nil {
			a.log.Errorf("error updating certificate %s status in storage: %w", certificateHeader.String(), err)
			return true
		}

		if !certificate.IsClosed() {
			a.log.Infof("certificate %s is still pending, elapsed time:%s ",
				certificateHeader.ID(), certificate.ElapsedTimeSinceCreation())
			thereArePendingCerts = true
		}
	}
	return thereArePendingCerts
}

// updateCertificate updates the certificate status in the storage
func (a *AggSender) updateCertificateStatus(ctx context.Context,
	localCert *types.CertificateInfo,
	agglayerCert *agglayer.CertificateHeader) error {
	if localCert.Status == agglayerCert.Status {
		return nil
	}
	a.log.Infof("certificate %s changed status from [%s] to [%s] elapsed time: %s full_cert (agglayer): %s",
		localCert.ID(), localCert.Status, agglayerCert.Status, localCert.ElapsedTimeSinceCreation(),
		agglayerCert.String())

	// That is a strange situation
	if agglayerCert.Status.IsOpen() && localCert.Status.IsClosed() {
		a.log.Warnf("certificate %s is reopened! from [%s] to [%s]",
			localCert.ID(), localCert.Status, agglayerCert.Status)
	}

	localCert.Status = agglayerCert.Status
	localCert.UpdatedAt = uint32(time.Now().UTC().Unix())
	if err := a.storage.UpdateCertificate(ctx, *localCert); err != nil {
		a.log.Errorf("error updating certificate %s status in storage: %w", agglayerCert.ID(), err)
		return fmt.Errorf("error updating certificate. Err: %w", err)
	}
	return nil
}

// checkLastCertificateFromAgglayer checks the last certificate from agglayer
func (a *AggSender) checkLastCertificateFromAgglayer(ctx context.Context) error {
	networkID := a.l2Syncer.OriginNetwork()
	a.log.Infof("recovery: checking last certificate from AggLayer for network %d", networkID)
	aggLayerLastCert, err := a.aggLayerClient.GetLatestKnownCertificateHeader(networkID)
	if err != nil {
		return fmt.Errorf("recovery: error getting latest known certificate header from agglayer: %w", err)
	}
	a.log.Infof("recovery: last certificate from AggLayer: %s", aggLayerLastCert.String())
	localLastCert, err := a.storage.GetLastSentCertificate()
	if err != nil {
		return fmt.Errorf("recovery: error getting last sent certificate from local storage: %w", err)
	}
	a.log.Infof("recovery: last certificate in storage: %s", localLastCert.String())

	// CASE 1: No certificates in local storage and agglayer
	if localLastCert == nil && aggLayerLastCert == nil {
		a.log.Info("recovery: No certificates in local storage and agglayer: initial state")
		return nil
	}
	// CASE 2: No certificates in local storage but agglayer has one
	if localLastCert == nil && aggLayerLastCert != nil {
		a.log.Info("recovery: No certificates in local storage but agglayer have one: recovery aggSender cert: %s",
			aggLayerLastCert.String())
		if _, err := a.updateLocalStorageWithAggLayerCert(ctx, aggLayerLastCert); err != nil {
			return fmt.Errorf("recovery: error updating local storage with agglayer certificate: %w", err)
		}
		return nil
	}
	// CASE 2.1: certificate in storage but not in agglayer
	// this is a non-sense, so throw an error
	if localLastCert != nil && aggLayerLastCert == nil {
		return fmt.Errorf("recovery: certificate exists in storage but not in agglayer. Inconsistency")
	}
	// CASE 3.1: the certificate on the agglayer has less height than the one stored in the local storage
	if aggLayerLastCert.Height < localLastCert.Height {
		return fmt.Errorf("recovery: the last certificate in the agglayer has less height (%d) "+
			"than the one in the local storage (%d)", aggLayerLastCert.Height, localLastCert.Height)
	}
	// CASE 3.2: aggsender stopped between sending to agglayer and storing to the local storage
	if aggLayerLastCert.Height == localLastCert.Height+1 {
		a.log.Infof("recovery: AggLayer has the next cert (height: %d), so is a recovery case: storing cert: %s",
			aggLayerLastCert.Height, aggLayerLastCert.String())
		// we need to store the certificate in the local storage.
		localLastCert, err = a.updateLocalStorageWithAggLayerCert(ctx, aggLayerLastCert)
		if err != nil {
			log.Errorf("recovery: error updating certificate: %s, reason: %w", aggLayerLastCert.String(), err)
			return fmt.Errorf("recovery: error updating certificate: %w", err)
		}
	}
	// CASE 4: AggSender and AggLayer are not on the same page
	// note: we don't need to check individual fields of the certificate
	// because CertificateID is a hash of all the fields
	if localLastCert.CertificateID != aggLayerLastCert.CertificateID {
		a.log.Errorf("recovery: Local certificate:\n %s \n is different from agglayer certificate:\n %s",
			localLastCert.String(), aggLayerLastCert.String())
		return fmt.Errorf("recovery: mismatch between local and agglayer certificates")
	}
	// CASE 5: AggSender and AggLayer are at same page
	// just update status
	err = a.updateCertificateStatus(ctx, localLastCert, aggLayerLastCert)
	if err != nil {
		a.log.Errorf("recovery: error updating status certificate: %s status: %w", aggLayerLastCert.String(), err)
		return fmt.Errorf("recovery: error updating certificate status: %w", err)
	}

	a.log.Infof("recovery: successfully checked last certificate from AggLayer for network %d", networkID)
	return nil
}

// updateLocalStorageWithAggLayerCert updates the local storage with the certificate from the AggLayer
func (a *AggSender) updateLocalStorageWithAggLayerCert(ctx context.Context,
	aggLayerCert *agglayer.CertificateHeader) (*types.CertificateInfo, error) {
	certInfo := NewCertificateInfoFromAgglayerCertHeader(aggLayerCert)
	a.log.Infof("setting initial certificate from AggLayer: %s", certInfo.String())
	return certInfo, a.storage.SaveLastSentCertificate(ctx, *certInfo)
}

// extractSignatureData extracts the R, S, and V from a 65-byte signature
func extractSignatureData(signature []byte) (r, s common.Hash, isOddParity bool, err error) {
	if len(signature) != signatureSize {
		err = errInvalidSignatureSize
		return
	}

	r = common.BytesToHash(signature[:32])   // First 32 bytes are R
	s = common.BytesToHash(signature[32:64]) // Next 32 bytes are S
	isOddParity = signature[64]%2 == 1       //nolint:mnd // Last byte is V

	return
}

func NewCertificateInfoFromAgglayerCertHeader(c *agglayer.CertificateHeader) *types.CertificateInfo {
	if c == nil {
		return nil
	}
	now := uint32(time.Now().UTC().Unix())
	meta := types.NewCertificateMetadataFromHash(c.Metadata)
	toBlock := meta.FromBlock + uint64(meta.Offset)
	createdAt := meta.CreatedAt

	if meta.Version < 1 {
		toBlock = meta.ToBlock
		createdAt = now
	}

	res := &types.CertificateInfo{
		Height:            c.Height,
		CertificateID:     c.CertificateID,
		NewLocalExitRoot:  c.NewLocalExitRoot,
		FromBlock:         meta.FromBlock,
		ToBlock:           toBlock,
		Status:            c.Status,
		CreatedAt:         createdAt,
		UpdatedAt:         now,
		SignedCertificate: "na/agglayer header",
	}
	if c.PreviousLocalExitRoot != nil {
		res.PreviousLocalExitRoot = c.PreviousLocalExitRoot
	}
	return res
}

// getLastSentBlockAndRetryCount returns the last sent block of the last sent certificate
// if there is no previosly sent certificate, it returns 0 and 0
func getLastSentBlockAndRetryCount(lastSentCertificateInfo *types.CertificateInfo) (uint64, int) {
	if lastSentCertificateInfo == nil {
		return 0, 0
	}

	retryCount := 0
	lastSentBlock := lastSentCertificateInfo.ToBlock

	if lastSentCertificateInfo.Status == agglayer.InError {
		// if the last certificate was in error, we need to resend it
		// from the block before the error
		if lastSentCertificateInfo.FromBlock > 0 {
			lastSentBlock = lastSentCertificateInfo.FromBlock - 1
		}

		retryCount = lastSentCertificateInfo.RetryCount + 1
	}

	return lastSentBlock, retryCount
}

func ConnectTree(dbPath string) (*sql.DB, *tree.AppendOnlyTree, error) {
	db, err := cdkdb.NewSQLiteDB(dbPath)
	if err != nil {
		return nil, nil, err
	}

	exitTree := tree.NewAppendOnlyTree(db, "")
	return db, exitTree, nil
}

func (a *AggSender) GetRootByIndex(ctx context.Context, index uint32, tx cdkdb.Txer) (treeTypes.Root, error) {
	var root treeTypes.Root
	if err := meddler.QueryRow(
		tx, &root,
		"SELECT * FROM root WHERE position = $1;",
		index,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return root, errors.New("not found")
		}
		return root, err
	}
	return root, nil
}
