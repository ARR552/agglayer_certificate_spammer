package main

import (
	"os"

	"github.com/0xPolygon/cdk/config"
	"github.com/0xPolygon/cdk/log"
	"github.com/urfave/cli/v2"
)

const (
	urlFlagName               = "url"
	validSignatureFlagName    = "valid-signature"
	privateKeyFlagName        = "private-key"
	emptyCertFlagName         = "empty-cert"
	addFakeBridgeFlagName     = "add-fake-bridge"
	storeCertificateFlagName  = "store-certificate"
	networkIDFlagName         = "network-id"
	singleCertFlagName        = "single-cert"
	certHeightFlagName        = "height"
	randomGlobalIndexFlagName = "random-global-index"
)

var (
	configFileFlag = cli.StringSliceFlag{
		Name:     config.FlagCfg,
		Aliases:  []string{"c"},
		Usage:    "Configuration file(s)",
		Required: false,
	}
	disableDefaultConfigVars = cli.BoolFlag{
		Name:     config.FlagDisableDefaultConfigVars,
		Aliases:  []string{"d"},
		Usage:    "Disable default configuration variables, all of them must be defined on config files",
		Required: false,
	}
	urlFlag = cli.StringFlag{
		Name:     urlFlagName,
		Aliases:  []string{"u"},
		Usage:    "Defines the url of the agglayer",
		Required: true,
	}
	validSignatureFlag = cli.BoolFlag{
		Name:     validSignatureFlagName,
		Aliases:  []string{"vs"},
		Usage:    "Defines if the signature must be valid",
		Required: false,
	}
	privateKeyFlag = cli.StringFlag{
		Name:     privateKeyFlagName,
		Aliases:  []string{"pk"},
		Usage:    "Defines the private key. If it is set is used to sign the certificate. If not, a random account is used to sign the certificate if valid-signature is enabled. If it is disabled, a random signature is used.",
		Required: false,
	}
	emptyCertificateFlag = cli.BoolFlag{
		Name:     emptyCertFlagName,
		Aliases:  []string{"empty"},
		Usage:    "Defines if the certificate must be empty. Without bridges and claims",
		Required: false,
	}
	addFakeBridgeFlag = cli.BoolFlag{
		Name:     addFakeBridgeFlagName,
		Aliases:  []string{"fake-bridge"},
		Usage:    "Defines if the certificate must include an extra fake bridge in L2 to try to cheat the agglayer",
		Required: false,
	}
	storeCertificateFlag = cli.BoolFlag{
		Name:     storeCertificateFlagName,
		Aliases:  []string{"store-cert"},
		Usage:    "Defines if the certificate must be stored in the database",
		Required: false,
	}
	networkIDFlag = cli.UintFlag{
		Name:     networkIDFlagName,
		Aliases:  []string{"net"},
		Usage:    "Defines the network ID of the certificate",
		Required: false,
	}
	singleCertFlag = cli.BoolFlag{
		Name:     singleCertFlagName,
		Aliases:  []string{"sigle"},
		Usage:    "Force to send only one certificate",
		Required: false,
	}
	certHeightFlag = cli.StringFlag{
		Name:     certHeightFlagName,
		Usage:    "Sets the height of the certificate",
		Required: false,
	}
	randomGlobalIndexFlag = cli.BoolFlag{
		Name:     randomGlobalIndexFlagName,
		Aliases:  []string{"random-gi"},
		Usage:    "Force the GlobalIndex to be fully random. MainnetFlag (true) and rollupIndex (!= 0) at the same time",
		Required: false,
	}
)

func main() {
	app := cli.NewApp()
	app.Name = "Aglayer certificate spammer"
	app.Commands = []*cli.Command{
		{
			Name:    "valid-certs",
			Aliases: []string{},
			Usage:   "Generate and send valid certificates",
			Action:  sendValidCerts,
			Flags: []cli.Flag{
				&configFileFlag,
				&disableDefaultConfigVars,
				&emptyCertificateFlag,
				&addFakeBridgeFlag,
				&storeCertificateFlag,
				&singleCertFlag,
			},
		},
		{
			Name:    "invalid-signature-certs",
			Aliases: []string{},
			Usage:   "Generate and send certificates with invalid signatures",
			Action:  sendInvalidSignatureCerts,
			Flags: []cli.Flag{
				&configFileFlag,
				&disableDefaultConfigVars,
				&emptyCertificateFlag,
				&addFakeBridgeFlag,
				&storeCertificateFlag,
				&singleCertFlag,
			},
		},
		{
			Name:    "random-certs",
			Aliases: []string{},
			Usage:   "Generate and send random certificates",
			Action:  randomCerts,
			Flags: []cli.Flag{
				&urlFlag,
				&validSignatureFlag,
				&privateKeyFlag,
				&emptyCertificateFlag,
				&networkIDFlag,
				&certHeightFlag,
				&randomGlobalIndexFlag,
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}
