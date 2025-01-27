package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"os"
	"os/signal"
	"runtime"

	"github.com/0xPolygon/cdk"
	"github.com/0xPolygon/cdk/agglayer"
	"github.com/0xPolygon/cdk/aggsender"
	"github.com/0xPolygon/cdk/bridgesync"
	"github.com/0xPolygon/cdk/common"
	"github.com/0xPolygon/cdk/config"
	"github.com/0xPolygon/cdk/etherman"
	"github.com/0xPolygon/cdk/l1infotreesync"
	"github.com/0xPolygon/cdk/log"
	"github.com/0xPolygon/cdk/reorgdetector"
	spammerAggsender "github.com/ARR552/agglayer_certificate_spammer/aggsender"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfave/cli/v2"
)

func runBridgeSyncL2(
	ctx context.Context,
	cfg bridgesync.Config,
	reorgDetectorL2 *reorgdetector.ReorgDetector,
	l2Client *ethclient.Client,
	rollupID uint32,
) (*bridgesync.BridgeSync, error) {
	bridgeSyncL2, err := bridgesync.NewL2(
		ctx,
		cfg.DBPath,
		cfg.BridgeAddr,
		cfg.SyncBlockChunkSize,
		etherman.BlockNumberFinality(cfg.BlockFinality),
		reorgDetectorL2,
		l2Client,
		cfg.InitialBlockNum,
		cfg.WaitForNewBlocksPeriod.Duration,
		cfg.RetryAfterErrorPeriod.Duration,
		cfg.MaxRetryAttemptsAfterError,
		rollupID,
		true,
	)
	if err != nil {
		log.Errorf("error creating bridgeSyncL2: %s", err)
		return nil, err
	}
	go bridgeSyncL2.Start(ctx)

	return bridgeSyncL2, nil
}

func runReorgDetectorL2(
	ctx context.Context,
	l2Client *ethclient.Client,
	cfg *reorgdetector.Config,
) (*reorgdetector.ReorgDetector, chan error, error) {
	rd, err := reorgdetector.New(l2Client, *cfg)
	if err != nil {
		log.Error(err)
		return nil, nil, err
	}
	errChan := make(chan error)
	go func() {
		if err := rd.Start(ctx); err != nil {
			errChan <- err
		}
		close(errChan)
	}()

	return rd, errChan, nil
}

func runReorgDetectorL1(
	ctx context.Context,
	l1Client *ethclient.Client,
	cfg *reorgdetector.Config,
) (*reorgdetector.ReorgDetector, chan error, error) {
	rd, err := reorgdetector.New(l1Client, *cfg)
	if err != nil {
		log.Error(err)
		return nil, nil, err
	}

	errChan := make(chan error)
	go func() {
		if err := rd.Start(ctx); err != nil {
			errChan <- err
		}
		close(errChan)
	}()

	return rd, errChan, nil
}

func runL1InfoTreeSyncer(
	ctx context.Context,
	cfg config.Config,
	l1Client *ethclient.Client,
	reorgDetector *reorgdetector.ReorgDetector,
) (*l1infotreesync.L1InfoTreeSync, error) {
	l1InfoTreeSync, err := l1infotreesync.New(
		ctx,
		cfg.L1InfoTreeSync.DBPath,
		cfg.L1InfoTreeSync.GlobalExitRootAddr,
		cfg.L1InfoTreeSync.RollupManagerAddr,
		cfg.L1InfoTreeSync.SyncBlockChunkSize,
		etherman.BlockNumberFinality(cfg.L1InfoTreeSync.BlockFinality),
		reorgDetector,
		l1Client,
		cfg.L1InfoTreeSync.WaitForNewBlocksPeriod.Duration,
		cfg.L1InfoTreeSync.InitialBlock,
		cfg.L1InfoTreeSync.RetryAfterErrorPeriod.Duration,
		cfg.L1InfoTreeSync.MaxRetryAttemptsAfterError,
		l1infotreesync.FlagNone,
	)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	go l1InfoTreeSync.Start(ctx)

	return l1InfoTreeSync, nil
}

func createAggSender(
	ctx context.Context,
	cfg aggsender.Config,
	l1EthClient *ethclient.Client,
	l1InfoTreeSync *l1infotreesync.L1InfoTreeSync,
	l2Syncer *bridgesync.BridgeSync,
	bridgeDB string,
	sequencerPrivateKey *ecdsa.PrivateKey,
) (*spammerAggsender.AggSender, error) {
	logger := log.WithFields("module", "spammer_aggsender")
	agglayerClient := agglayer.NewAggLayerClient(cfg.AggLayerURL)
	blockNotifier, err := aggsender.NewBlockNotifierPolling(l1EthClient, aggsender.ConfigBlockNotifierPolling{
		BlockFinalityType:     etherman.BlockNumberFinality(cfg.BlockFinality),
		CheckNewBlockInterval: aggsender.AutomaticBlockInterval,
	}, logger, nil)
	if err != nil {
		return nil, err
	}

	notifierCfg, err := aggsender.NewConfigEpochNotifierPerBlock(agglayerClient, cfg.EpochNotificationPercentage)
	if err != nil {
		return nil, fmt.Errorf("cant generate config for Epoch Notifier because: %w", err)
	}
	epochNotifier, err := aggsender.NewEpochNotifierPerBlock(
		blockNotifier,
		logger,
		*notifierCfg, nil)
	if err != nil {
		return nil, err
	}
	log.Infof("Starting blockNotifier: %s", blockNotifier.String())
	go blockNotifier.Start(ctx)
	log.Infof("Starting epochNotifier: %s", epochNotifier.String())
	go epochNotifier.Start(ctx)

	return spammerAggsender.New(ctx, logger, cfg, agglayerClient, l1InfoTreeSync, l2Syncer, epochNotifier, sequencerPrivateKey, bridgeDB)
}

func sendValidCerts(ctx *cli.Context) error {
	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}
	emptyCert := ctx.Bool(emptyCertFlagName)
	addFakeBridge := ctx.Bool(addFakeBridgeFlagName)
	storeCertificate := ctx.Bool(storeCertificateFlagName)

	log.Init(cfg.Log)

	log.Infow("Starting application",
		"gitRevision", cdk.GitRev,
		"gitBranch", cdk.GitBranch,
		"goVersion", runtime.Version(),
		"built", cdk.BuildDate,
		"os/arch", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	)

	urlRPCL1 := cfg.Etherman.URL
	log.Debugf("dialing L1 client at: %s", urlRPCL1)
	l1Client, err := ethclient.Dial(urlRPCL1)
	if err != nil {
		log.Errorf("failed to create client for L1 using URL: %s. Err:%v", urlRPCL1, err)
		return err
	}
	urlRPCL2 := getL2RPCUrl(cfg)
	log.Infof("dialing L2 client at: %s", urlRPCL2)
	l2Client, err := ethclient.Dial(urlRPCL2)
	if err != nil {
		log.Error(err)
		return err
	}
	reorgDetectorL1, errChanL1, err := runReorgDetectorL1(ctx.Context, l1Client, &cfg.ReorgDetectorL1)
	if err != nil {
		log.Error("Error from ReorgDetectorL1: ", err)
		return err
	}
	go func() {
		if err := <-errChanL1; err != nil {
			log.Fatal("Error from ReorgDetectorL1: ", err)
		}
	}()

	reorgDetectorL2, errChanL2, err := runReorgDetectorL2(ctx.Context, l2Client, &cfg.ReorgDetectorL2)
	if err != nil {
		log.Error("Error from ReorgDetectorL2: ", err)
		return err
	}
	go func() {
		if err := <-errChanL2; err != nil {
			log.Fatal("Error from ReorgDetectorL2: ", err)
		}
	}()

	rollupID, err := etherman.GetRollupID(cfg.NetworkConfig.L1Config, cfg.NetworkConfig.L1Config.ZkEVMAddr, l1Client)
	if err != nil {
		log.Error(err)
		return err
	}
	l1InfoTreeSync, err := runL1InfoTreeSyncer(ctx.Context, *cfg, l1Client, reorgDetectorL1)
	if err != nil {
		log.Error(err)
		return err
	}
	l2BridgeSync, err := runBridgeSyncL2(ctx.Context, cfg.BridgeL2Sync, reorgDetectorL2, l2Client, rollupID)
	if err != nil {
		log.Error(err)
		return err
	}
	sequencerPrivateKey, err := common.NewKeyFromKeystore(cfg.AggSender.AggsenderPrivateKey)
	if err != nil {
		log.Error(err)
		return err
	}
	aggsender, err := createAggSender(
		ctx.Context,
		cfg.AggSender,
		l1Client,
		l1InfoTreeSync,
		l2BridgeSync,
		cfg.BridgeL2Sync.DBPath,
		sequencerPrivateKey,
	)
	if err != nil {
		log.Error(err)
		return err
	}
	go aggsender.Start(ctx.Context, emptyCert, addFakeBridge, storeCertificate)
	waitSignal(nil)

	return nil
}

func getL2RPCUrl(c *config.Config) string {
	if c.AggSender.URLRPCL2 != "" {
		return c.AggSender.URLRPCL2
	}

	return c.AggOracle.EVMSender.URLRPCL2
}

func waitSignal(cancelFuncs []context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	for sig := range signals {
		switch sig {
		case os.Interrupt, os.Kill:
			log.Info("terminating application gracefully...")

			exitStatus := 0
			for _, cancel := range cancelFuncs {
				cancel()
			}
			os.Exit(exitStatus)
		}
	}
}
