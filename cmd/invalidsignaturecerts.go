package main

import (
	"crypto/ecdsa"
	"fmt"
	"runtime"

	"github.com/0xPolygon/cdk"
	"github.com/0xPolygon/cdk/config"
	"github.com/0xPolygon/cdk/etherman"
	"github.com/0xPolygon/cdk/log"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfave/cli/v2"
)

func sendInvalidSignatureCerts(ctx *cli.Context) error {
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
	randomPrivateKey, err := crypto.GenerateKey()
	if err != nil {
		log.Error(err)
		return err
	}
	log.Info("Random Private Key generated:", hexutil.Encode(crypto.FromECDSA(randomPrivateKey)))

	publicKey := randomPrivateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Error("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
		return fmt.Errorf("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
	}
	log.Info("Generated wallet Address:", crypto.PubkeyToAddress(*publicKeyECDSA).Hex())
	aggsender, err := createAggSender(
		ctx.Context,
		cfg.AggSender,
		l1Client,
		l1InfoTreeSync,
		l2BridgeSync,
		cfg.BridgeL2Sync.DBPath,
		randomPrivateKey,
	)
	if err != nil {
		log.Error(err)
		return err
	}
	go aggsender.Start(ctx.Context, emptyCert, addFakeBridge, storeCertificate)
	waitSignal(nil)

	return nil
}
