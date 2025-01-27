package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"fmt"
	"math/big"
	mathrand "math/rand/v2"
	"strings"

	"github.com/0xPolygon/cdk/agglayer"
	"github.com/0xPolygon/cdk/log"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfave/cli/v2"
)

func genRandomCert(emptyCert bool) (*agglayer.Certificate, error) {
	var (
		bridgeExits         []*agglayer.BridgeExit
		importedBridgeExits []*agglayer.ImportedBridgeExit
		err                 error
	)
	if !emptyCert {
		log.Info("Generating random bridges and claims...")
		bridgeExits, importedBridgeExits, err = generateBridgesAndClaims()
		if err != nil {
			log.Error("error generating bridges and claims. Error: ", err)
			return nil, err
		}
	} else {
		log.Info("Generating empty certificate...")
	}

	cert := agglayer.Certificate{
		NetworkID:           mathrand.Uint32(),
		Height:              mathrand.Uint64(),
		PrevLocalExitRoot:   randomHash(),
		NewLocalExitRoot:    randomHash(),
		BridgeExits:         bridgeExits,
		ImportedBridgeExits: importedBridgeExits,
		Metadata:            randomHash(),
	}
	return &cert, nil
}

// randomHash generates a random hash.
func randomHash() common.Hash {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		log.Error(err)
		return common.Hash{}
	}
	return common.BytesToHash(bytes)
}

// randomAddress generates a random address.
func randomAddress() common.Address {
	bytes := make([]byte, 20)
	if _, err := rand.Read(bytes); err != nil {
		log.Error(err)
		return common.Address{}
	}
	return common.BytesToAddress(bytes)
}

func randomCerts(ctx *cli.Context) error {
	url := ctx.String(urlFlagName)
	privateKey := ctx.String(privateKeyFlagName)
	validSignature := ctx.Bool(validSignatureFlagName)
	emptyCert := ctx.Bool(emptyCertFlagName)
	cert, err := genRandomCert(emptyCert)
	if err != nil {
		log.Error(err)
		return err
	}
	var signedCert *agglayer.SignedCertificate
	if !validSignature {
		log.Info("Generating random signature...")
		signedCert = &agglayer.SignedCertificate{
			Certificate: cert,
			Signature: &agglayer.Signature{
				R:         randomHash(),
				S:         randomHash(),
				OddParity: mathrand.UintN(1) == 0,
			},
		}
	} else {
		var privKey *ecdsa.PrivateKey
		if privateKey == "" {
			privKey, err = crypto.GenerateKey()
			if err != nil {
				log.Error(err)
				return err
			}
			log.Info("Random Private Key generated:", hexutil.Encode(crypto.FromECDSA(privKey)))

			publicKey := privKey.Public()
			publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
			if !ok {
				log.Error("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
				return fmt.Errorf("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
			}
			log.Info("Generated wallet Address:", crypto.PubkeyToAddress(*publicKeyECDSA).Hex())
			signedCert, err = signCertificate(cert, privKey)
			if err != nil {
				log.Error("error signing the certificate. Error: ", err)
				return err
			}
		} else {
			privKey, err = crypto.HexToECDSA(strings.TrimPrefix(privateKey, "0x"))
			if err != nil {
				log.Fatal(err)
			}
			publicKey := privKey.Public()
			publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
			if !ok {
				log.Error("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
				return fmt.Errorf("cannot assert type: publicKey is not of type *ecdsa.PublicKey")
			}
			log.Info("Imported wallet Address:", crypto.PubkeyToAddress(*publicKeyECDSA).Hex())
			signedCert, err = signCertificate(cert, privKey)
			if err != nil {
				log.Error("error signing the certificate. Error: ", err)
				return err
			}
		}
	}
	err = sendCert(url, signedCert)
	if err != nil {
		log.Error(err)
		return err
	}
	return nil
}

func sendCert(url string, cert *agglayer.SignedCertificate) error {
	log.Debugf("%+v\n", cert.Certificate)
	hash, err := agglayer.NewAggLayerClient(url).SendCertificate(cert)
	if err != nil {
		log.Error(err)
		return err
	}
	log.Info("Certificate sent with hash: ", hash.String())
	return nil
}

// signCertificate signs a certificate with the sequencer key
func signCertificate(certificate *agglayer.Certificate, privateKey *ecdsa.PrivateKey) (*agglayer.SignedCertificate, error) {
	hashToSign := certificate.HashToSign()

	signature, err := crypto.Sign(hashToSign.Bytes(), privateKey)
	if err != nil {
		return nil, err
	}

	log.Infof("Signed certificate. sequencer address: %s. New local exit root: %s Hash signed: %s",
		crypto.PubkeyToAddress(privateKey.PublicKey).String(),
		common.BytesToHash(certificate.NewLocalExitRoot[:]).String(),
		hashToSign.String(),
	)

	const signatureSize = 65
	if len(signature) != signatureSize {
		return nil, fmt.Errorf("invalid signature size")
	}

	r := common.BytesToHash(signature[:32])   // First 32 bytes are R
	s := common.BytesToHash(signature[32:64]) // Next 32 bytes are S
	isOddParity := signature[64]%2 == 1       //nolint:mnd // Last byte is V

	return &agglayer.SignedCertificate{
		Certificate: certificate,
		Signature: &agglayer.Signature{
			R:         r,
			S:         s,
			OddParity: isOddParity,
		},
	}, nil
}

func generateBridgesAndClaims() ([]*agglayer.BridgeExit, []*agglayer.ImportedBridgeExit, error) {
	amount, err := rand.Int(rand.Reader, big.NewInt(1000000000000000000))
	if err != nil {
		return nil, nil, err
	}
	var bridgeExits []*agglayer.BridgeExit
	maxBridges := mathrand.UintN(8)
	for i := 0; i < int(maxBridges); i++ {
		bridgeExits = append(bridgeExits, &agglayer.BridgeExit{
			LeafType: agglayer.LeafType(mathrand.UintN(2)),
			TokenInfo: &agglayer.TokenInfo{
				OriginNetwork:      mathrand.Uint32(),
				OriginTokenAddress: randomAddress(),
			},
			DestinationNetwork: mathrand.Uint32(),
			DestinationAddress: randomAddress(),
			Amount:             amount,
			IsMetadataHashed:   mathrand.UintN(1) == 0,
			Metadata:           randomHash().Bytes(),
		})
	}
	var importedBridgeExits []*agglayer.ImportedBridgeExit
	for i := 0; i < int(maxBridges); i++ {
		importedBridgeExits = append(importedBridgeExits, &agglayer.ImportedBridgeExit{
			BridgeExit: &agglayer.BridgeExit{
				LeafType: agglayer.LeafType(mathrand.UintN(2)),
				TokenInfo: &agglayer.TokenInfo{
					OriginNetwork:      mathrand.Uint32(),
					OriginTokenAddress: randomAddress(),
				},
				DestinationNetwork: mathrand.Uint32(),
				DestinationAddress: randomAddress(),
				Amount:             amount,
				IsMetadataHashed:   mathrand.UintN(1) == 0,
				Metadata:           randomHash().Bytes(),
			},
			ClaimData: generateClaimData(),
			GlobalIndex: &agglayer.GlobalIndex{
				MainnetFlag: mathrand.UintN(1) == 0,
				RollupIndex: mathrand.Uint32(),
				LeafIndex:   mathrand.Uint32(),
			},
		})
	}
	return bridgeExits, importedBridgeExits, nil
}

func generateMainnetClaim() agglayer.ClaimFromMainnnet {
	mainnet := agglayer.ClaimFromMainnnet{
		ProofLeafMER: &agglayer.MerkleProof{
			Root:  randomHash(),
			Proof: [32]common.Hash{},
		},
		ProofGERToL1Root: &agglayer.MerkleProof{
			Root:  randomHash(),
			Proof: [32]common.Hash{},
		},
		L1Leaf: &agglayer.L1InfoTreeLeaf{
			L1InfoTreeIndex: mathrand.Uint32(),
			RollupExitRoot:  randomHash(),
			MainnetExitRoot: randomHash(),
			Inner: &agglayer.L1InfoTreeLeafInner{
				GlobalExitRoot: randomHash(),
				BlockHash:      randomHash(),
				Timestamp:      mathrand.Uint64(),
			},
		},
	}
	for i := 0; i < 32; i++ {
		mainnet.ProofLeafMER.Proof[i] = randomHash()
		mainnet.ProofGERToL1Root.Proof[i] = randomHash()
	}
	return mainnet
}

func generateRollupClaim() agglayer.ClaimFromRollup {
	rollup := agglayer.ClaimFromRollup{
		ProofLeafLER: &agglayer.MerkleProof{
			Root:  randomHash(),
			Proof: [32]common.Hash{},
		},
		ProofLERToRER: &agglayer.MerkleProof{
			Root:  randomHash(),
			Proof: [32]common.Hash{},
		},
		ProofGERToL1Root: &agglayer.MerkleProof{
			Root:  randomHash(),
			Proof: [32]common.Hash{},
		},
		L1Leaf: &agglayer.L1InfoTreeLeaf{
			L1InfoTreeIndex: mathrand.Uint32(),
			RollupExitRoot:  randomHash(),
			MainnetExitRoot: randomHash(),
			Inner: &agglayer.L1InfoTreeLeafInner{
				GlobalExitRoot: randomHash(),
				BlockHash:      randomHash(),
				Timestamp:      mathrand.Uint64(),
			},
		},
	}
	for i := 0; i < 32; i++ {
		rollup.ProofLeafLER.Proof[i] = randomHash()
		rollup.ProofLERToRER.Proof[i] = randomHash()
		rollup.ProofGERToL1Root.Proof[i] = randomHash()
	}
	return rollup
}

func generateClaimData() agglayer.Claim {
	var claimData agglayer.Claim
	if mathrand.UintN(1) == 0 {
		rollup := generateRollupClaim()
		claimData = &rollup
	} else {
		mainnet := generateMainnetClaim()
		claimData = &mainnet
	}
	return claimData
}
