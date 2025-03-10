/*
 * Copyright 2020-2021, Offchain Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	golog "log"
	"math/big"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/staker"
	"github.com/offchainlabs/arbitrum/packages/arb-util/arblog"
	"github.com/offchainlabs/arbitrum/packages/arb-util/ethbridgecontracts"
	"github.com/offchainlabs/arbitrum/packages/arb-util/ethutils"
	"github.com/offchainlabs/arbitrum/packages/arb-util/transactauth"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/ethclient"
	gethlog "github.com/ethereum/go-ethereum/log"
	"github.com/pkg/errors"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/pkgerrors"

	"github.com/offchainlabs/arbitrum/packages/arb-node-core/cmdhelp"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/ethbridge"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/metrics"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/monitor"
	"github.com/offchainlabs/arbitrum/packages/arb-node-core/nodehealth"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/aggregator"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/batcher"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/nitroexport"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/rpc"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/txdb"
	"github.com/offchainlabs/arbitrum/packages/arb-rpc-node/web3"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcastclient"
	"github.com/offchainlabs/arbitrum/packages/arb-util/broadcaster"
	"github.com/offchainlabs/arbitrum/packages/arb-util/common"
	"github.com/offchainlabs/arbitrum/packages/arb-util/configuration"
)

var logger zerolog.Logger

var pprofMux *http.ServeMux

const largeChannelBuffer = 200

const (
	failLimit            = 6
	checkFrequency       = time.Second * 30
	blockCheckCountDelay = 5
)

func init() {
	pprofMux = http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
}

func main() {
	// Enable line numbers in logging
	golog.SetFlags(golog.LstdFlags | golog.Lshortfile)

	// Print stack trace when `.Error().Stack().Err(err).` is added to zerolog call
	zerolog.ErrorStackMarshaler = pkgerrors.MarshalStack

	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Print line number that log was created on
	logger = arblog.Logger.With().Str("component", "arb-node").Logger()

	if err := startup(); err != nil {
		logger.Error().Err(err).Msg("Error running node")
		fmt.Printf("\nNotice: %s\n\n", err.Error())
	}
}

func printSampleUsage() {
	fmt.Printf("\n")
	fmt.Printf("Sample usage:                  arb-node --conf=<filename> \n")
	fmt.Printf("          or:  forwarder node: arb-node --l1.url=<L1 RPC> [optional arguments]\n\n")
	fmt.Printf("          or: aggregator node: arb-node --l1.url=<L1 RPC> --node.type=aggregator [optional arguments] %s\n", cmdhelp.WalletArgsString)
	fmt.Printf("          or:       sequencer: arb-node --l1.url=<L1 RPC> --node.type=sequencer [optional arguments] %s\n", cmdhelp.WalletArgsString)
}

func getKeystore(
	config *configuration.Config,
	walletConfig *configuration.Wallet,
	l1ChainId *big.Int,
	signerRequired bool,
) (*bind.TransactOpts, func([]byte) ([]byte, error), error) {
	return cmdhelp.GetKeystore(config, walletConfig, l1ChainId, signerRequired)
}

func startup() error {
	ctx, cancelFunc, cancelChan := cmdhelp.CreateLaunchContext()
	defer cancelFunc()

	config, walletConfig, l1Client, l1ChainId, err := configuration.ParseNode(ctx)
	if err != nil || len(config.Persistent.GlobalConfig) == 0 || len(config.L1.URL) == 0 ||
		len(config.Rollup.Address) == 0 || len(config.BridgeUtilsAddress) == 0 ||
		((config.Node.Type() != configuration.SequencerNodeType) && len(config.Node.Sequencer.Lockout.Redis) != 0) ||
		((len(config.Node.Sequencer.Lockout.Redis) == 0) != (len(config.Node.Sequencer.Lockout.SelfRPCURL) == 0)) {
		printSampleUsage()
		if err != nil && !strings.Contains(err.Error(), "help requested") {
			fmt.Printf("\n%s\n", err.Error())
		}

		return nil
	}

	logger.Info().Str("database", config.GetDatabasePath()).Send()

	if config.Core.Database.Metadata {
		return cmdhelp.PrintDatabaseMetadata(config.GetDatabasePath(), &config.Core)
	}

	var validatorAuth *bind.TransactOpts
	if config.Node.Type() == configuration.ValidatorNodeType && config.Validator.Strategy() != configuration.WatchtowerStrategy {
		// Create key if needed before opening database
		validatorAuth, _, err = getKeystore(config, walletConfig, l1ChainId, false)
		if err != nil {
			return err
		}

		if config.Validator.OnlyCreateWalletContract {
			// Just create validator smart wallet if needed then exit
			_, err := startValidator(ctx, config, walletConfig, l1Client, validatorAuth, nil)
			if err != nil {
				return err
			}

			return errors.New("missing message when only-create-wallet-contract set")
		}
	} else {
		// No wallet, so just use empty auth object
		validatorAuth = &bind.TransactOpts{}
	}

	if config.BridgeUtilsAddress == "" {
		return errors.Errorf("Missing --bridge-utils-address")
	}
	if config.Persistent.Chain == "" {
		return errors.Errorf("Missing --persistent.chain")
	}
	if config.Rollup.Address == "" {
		return errors.Errorf("Missing --rollup.address")
	}
	if config.Node.ChainID == 0 {
		return errors.Errorf("Missing --node.chain-id")
	}
	if config.Rollup.Machine.Filename == "" {
		return errors.Errorf("Missing --rollup.machine.filename")
	}

	rpcMode := config.Node.Forwarder.RpcMode()
	if config.Node.Type() == configuration.ForwarderNodeType {
		if rpcMode == configuration.NonMutatingRpcMode {
			config.Node.Forwarder.Target = ""
		} else {
			if config.Node.Forwarder.Target == "" {
				return errors.New("Forwarder node needs --node.forwarder.target")
			}

			if rpcMode == configuration.GanacheRpcMode || rpcMode == configuration.UnknownRpcMode {
				return errors.Errorf("Unrecognized RPC mode %s", config.Node.Forwarder.RpcModeImpl)
			}
		}
	} else if config.Node.Type() == configuration.AggregatorNodeType {
		if config.Node.Aggregator.InboxAddress == "" {
			return errors.New("Aggregator node needs --node.aggregator.inbox-address")
		}
	} else if config.Node.Type() == configuration.SequencerNodeType {
		// Sequencer always waits
		config.WaitToCatchUp = true
	} else if config.Node.Type() == configuration.ValidatorNodeType {
		if config.Validator.StrategyImpl == "" {
			return errors.New("Missing --validator.strategy, should be Watchtower, Defensive, StakeLatest, or MakeNodes")
		} else if config.Validator.Strategy() == configuration.UnknownStrategy {
			return errors.Errorf("Unrecognized --validator.strategy %s, should be Watchtower, Defensive, StakeLatest, or MakeNodes", config.Validator.StrategyImpl)
		}
	} else {
		return errors.Errorf("Unrecognized node type %s", config.Node.TypeImpl)
	}

	if config.Node.Sequencer.Dangerous != (configuration.SequencerDangerous{}) {
		logger.
			Error().
			Interface("dangerousSequencerConfig", config.Node.Sequencer.Dangerous).
			Msg("sequencer starting up with dangerous options enabled!")
	}

	defer logger.Log().Msg("Cleanly shutting down node")

	if err := cmdhelp.ParseLogFlags(&config.Log.RPC, &config.Log.Core, gethlog.StreamHandler(os.Stderr, gethlog.JSONFormat())); err != nil {
		return err
	}

	if config.PProfEnable {
		go func() {
			err := http.ListenAndServe("localhost:8081", pprofMux)
			logger.Error().Err(err).Msg("profiling server failed")
		}()
	}

	l2ChainId := new(big.Int).SetUint64(config.Node.ChainID)
	rollupAddress := common.HexToAddress(config.Rollup.Address)
	logger.Info().
		Hex("chainaddress", rollupAddress.Bytes()).
		Hex("chainid", l2ChainId.Bytes()).
		Str("type", config.Node.TypeImpl).
		Int64("fromBlock", config.Rollup.FromBlock).
		Msg("Launching arbitrum node")

	rollup, err := ethbridge.NewRollupWatcher(rollupAddress.ToEthAddress(), config.Rollup.FromBlock, l1Client, bind.CallOpts{})
	if err != nil {
		return err
	}

	if config.Node.Type() == configuration.ValidatorNodeType && config.Core.CheckpointMaxExecutionGas != 0 {
		logger.Warn().Msg("allowing for infinite core execution because running as validator")
		config.Core.CheckpointMaxExecutionGas = 0
	}

	mon, err := monitor.NewMonitor(config.GetDatabasePath(), &config.Core)
	if err != nil {
		return err
	}
	if err := mon.Initialize(config.Rollup.Machine.Filename); err != nil {
		return err
	}
	if err := mon.Start(); err != nil {
		return err
	}
	defer mon.Close()

	metricsConfig := metrics.NewMetricsConfig(config.MetricsServer, &config.Healthcheck.MetricsPrefix)

	var healthChan chan nodehealth.Log
	if config.Healthcheck.Enable {
		healthChan = make(chan nodehealth.Log, largeChannelBuffer)
		healthChan <- nodehealth.Log{Config: true, Var: "healthcheckMetrics", ValBool: config.Healthcheck.Metrics}
		healthChan <- nodehealth.Log{Config: true, Var: "disablePrimaryCheck", ValBool: !config.Healthcheck.Sequencer}
		healthChan <- nodehealth.Log{Config: true, Var: "disableOpenEthereumCheck", ValBool: !config.Healthcheck.L1Node}
		healthChan <- nodehealth.Log{Config: true, Var: "healthcheckRPC", ValStr: config.Healthcheck.Addr + ":" + config.Healthcheck.Port}

		if config.Node.Type() == configuration.ForwarderNodeType {
			healthChan <- nodehealth.Log{Config: true, Var: "primaryHealthcheckRPC", ValStr: config.Node.Forwarder.Target}
		}
		healthChan <- nodehealth.Log{Config: true, Var: "openethereumHealthcheckRPC", ValStr: config.L1.URL}
		nodehealth.Init(healthChan)
		go func() {
			err := nodehealth.StartNodeHealthCheck(ctx, healthChan, metricsConfig.Registry)
			if err != nil {
				logger.Error().Err(err).Msg("healthcheck server failed")
			}
		}()
	}

	// Message count is 1 based, seqNum is 0 based, so next seqNum to request is same as current message count
	currentMessageCount, err := mon.Core.GetMessageCount()
	if err != nil {
		return errors.Wrap(err, "can't get message count")
	}

	var sequencerFeed chan broadcaster.BroadcastFeedMessage
	broadcastClientErrChan := make(chan error)
	if len(config.Feed.Input.URLs) == 0 || len(config.Feed.Input.URLs[0]) == 0 {
		logger.Warn().Msg("Missing --feed.input.url so not subscribing to feed")
	} else if config.Node.Type() == configuration.ValidatorNodeType {
		logger.Info().Msg("Ignoring feed because running as validator")
	} else {
		sequencerFeed = make(chan broadcaster.BroadcastFeedMessage, 4096)
		for _, url := range config.Feed.Input.URLs {
			broadcastClient := broadcastclient.NewBroadcastClient(
				url,
				config.Node.ChainID,
				currentMessageCount,
				config.Feed.Input.Timeout,
				broadcastClientErrChan,
			)
			broadcastClient.ConnectInBackground(ctx, sequencerFeed)
		}
	}

	// InboxReader may fail to start if sequencer isn't up yet, so keep retrying
	var inboxReader *monitor.InboxReader
	var inboxReaderDone chan bool
	for {
		inboxReader, inboxReaderDone, err = mon.StartInboxReader(
			ctx,
			l1Client,
			common.HexToAddress(config.Rollup.Address),
			config.Rollup.FromBlock,
			common.HexToAddress(config.BridgeUtilsAddress),
			healthChan,
			sequencerFeed,
			config.Node.InboxReader,
		)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "arbcore thread aborted") {
			logger.Error().Err(err).Msg("aborting inbox reader start")
			break
		}
		logger.Warn().Err(err).
			Str("url", config.L1.URL).
			Str("rollup", config.Rollup.Address).
			Str("bridgeUtils", config.BridgeUtilsAddress).
			Int64("fromBlock", config.Rollup.FromBlock).
			Msg("failed to start inbox reader, waiting and retrying")

		select {
		case <-ctx.Done():
			return errors.New("ctx cancelled StartInboxReader retry loop")
		case <-time.After(5 * time.Second):
		}
	}

	if config.Core.CheckpointPruningMode != "off" {
		if err := cmdhelp.UpdatePrunePoint(ctx, rollup, mon.Core); err != nil {
			logger.Error().Err(err).Msg("error pruning database")
		}
	}

	var dataSigner func([]byte) ([]byte, error)
	var batcherMode rpc.BatcherMode
	var stakerManager *staker.Staker
	if config.Node.Type() == configuration.ValidatorNodeType {
		stakerManager, err = startValidator(ctx, config, walletConfig, l1Client, validatorAuth, mon)
		if err != nil {
			return err
		}
		batcherMode = rpc.ErrorBatcherMode{Error: errors.New("validator doesn't support transactions")}
	} else if config.Node.Type() == configuration.ForwarderNodeType {
		logger.Info().Str("forwardTxURL", config.Node.Forwarder.Target).Msg("Arbitrum node starting in forwarder mode")
		batcherMode = rpc.ForwarderBatcherMode{Config: config.Node.Forwarder}
	} else {
		var auth *bind.TransactOpts
		auth, dataSigner, err = getKeystore(config, walletConfig, l1ChainId, true)
		if err != nil {
			return err
		}

		if config.Node.Sequencer.Dangerous.DisableBatchPosting {
			logger.Info().Hex("from", auth.From.Bytes()).Msg("Arbitrum node with disabled batch posting")
		} else {
			logger.Info().Hex("from", auth.From.Bytes()).Msg("Arbitrum node submitting batches")
		}

		if err := ethbridge.WaitForBalance(
			ctx,
			l1Client,
			common.Address{},
			common.NewAddressFromEth(auth.From),
		); err != nil {
			return errors.Wrap(err, "error waiting for balance")
		}

		if config.Node.Type() == configuration.SequencerNodeType {
			batcherMode = rpc.SequencerBatcherMode{
				Auth:        auth,
				Core:        mon.Core,
				InboxReader: inboxReader,
			}
		} else {
			inboxAddress := common.HexToAddress(config.Node.Aggregator.InboxAddress)
			if config.Node.Aggregator.Stateful {
				batcherMode = rpc.StatefulBatcherMode{Auth: auth, InboxAddress: inboxAddress}
			} else {
				batcherMode = rpc.StatelessBatcherMode{Auth: auth, InboxAddress: inboxAddress}
			}
		}
	}

	nodeStore := mon.Storage.GetNodeStore()
	metricsConfig.RegisterNodeStoreMetrics(nodeStore)
	metricsConfig.RegisterArbCoreMetrics(mon.Core)
	db, txDBErrChan, err := txdb.New(ctx, mon.Core, nodeStore, &config.Node)
	if err != nil {
		return errors.Wrap(err, "error opening txdb")
	}
	defer db.Close()

	if config.WaitToCatchUp {
		inboxReader.WaitToCatchUp(ctx)
	}

	var batch batcher.TransactionBatcher
	var broadcasterErrChan chan error
	errChan := make(chan error, 1)
	if config.Node.Forwarder.Target != "" {
		for {
			batch, broadcasterErrChan, err = rpc.SetupBatcher(
				ctx,
				l1Client,
				rollupAddress,
				l2ChainId,
				db,
				time.Duration(config.Node.Aggregator.MaxBatchTime)*time.Second,
				batcherMode,
				dataSigner,
				config,
				walletConfig,
			)
			lockoutConf := config.Node.Sequencer.Lockout
			if err == nil {
				seqBatcher, ok := batch.(*batcher.SequencerBatcher)
				if lockoutConf.Redis != "" {
					// Setup the lockout. This will take care of the initial delayed sequence.
					batch, err = rpc.SetupLockout(ctx, seqBatcher, mon.Core, inboxReader, lockoutConf, errChan)
				} else if ok {
					// Ensure we sequence delayed messages before opening the RPC.
					err = seqBatcher.SequenceDelayedMessages(ctx, false)
				}
			}
			if err == nil {
				go batch.Start(ctx)
				break
			}
			if common.IsFatalError(err) {
				logger.Error().Err(err).Msg("aborting inbox reader start")
				break
			}
			logger.Warn().Err(err).Msg("failed to setup batcher, waiting and retrying")

			select {
			case <-ctx.Done():
				return errors.New("ctx cancelled setup batcher")
			case <-time.After(5 * time.Second):
			}
		}
	}

	var web3InboxReaderRef *monitor.InboxReader
	if config.Node.RPC.EnableL1Calls {
		web3InboxReaderRef = inboxReader
	}

	plugins := make(map[string]interface{})
	if config.Node.RPC.NitroExport.Enable {
		basedir := config.Node.RPC.NitroExport.BaseDir
		if basedir == "" {
			basedir = "nitroexport"
		}
		if !filepath.IsAbs(basedir) {
			basedir = path.Join(config.Persistent.Chain, basedir)
		}
		exportServer, err := nitroexport.NewExportRPCServer(ctx, db, mon.Core, basedir, l2ChainId)
		if err != nil {
			return err
		}
		plugins["arb"] = exportServer
	}

	srv := aggregator.NewServer(batch, l2ChainId, db)
	serverConfig := web3.ServerConfig{
		Mode:          rpcMode,
		MaxCallAVMGas: config.Node.RPC.MaxCallGas * 100, // Multiply by 100 for arb gas to avm gas conversion
		Tracing:       config.Node.RPC.Tracing,
		DevopsStubs:   config.Node.RPC.EnableDevopsStubs,
	}
	web3Server, err := web3.GenerateWeb3Server(srv, nil, serverConfig, mon.CoreConfig, plugins, web3InboxReaderRef)
	if err != nil {
		return err
	}
	go func() {
		err := rpc.LaunchPublicServer(ctx, web3Server, config.Node.RPC, config.Node.WS)
		if err != nil {
			errChan <- err
		}
	}()

	if config.Node.Type() == configuration.ForwarderNodeType && config.Node.Forwarder.Target != "" {
		go func() {
			clnt, err := ethclient.DialContext(ctx, config.Node.Forwarder.Target)
			if err != nil {
				logger.Warn().Err(err).Msg("failed to connect to forward target")
				clnt = nil
			}
			failCount := 0
			for {
				valid, err := checkBlockHash(ctx, clnt, db)
				if err != nil {
					logger.Warn().Err(err).Msg("failed to lookup blockhash for consistency check")
					clnt, err = ethclient.DialContext(ctx, config.Node.Forwarder.Target)
					if err != nil {
						logger.Warn().Err(err).Msg("failed to connect to forward target")
						clnt = nil
					}
				} else {
					if !valid {
						failCount++
					} else {
						failCount = 0
					}
				}
				if failCount >= failLimit {
					logger.Error().Msg("exiting due to repeated block hash mismatches")
					cancelFunc()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(checkFrequency):
				}
			}
		}()
	}

	if config.Core.CheckpointPruningMode != "off" {
		ticker := time.NewTicker(time.Minute)
		go func() {
			defer ticker.Stop()
			for {
				if err := cmdhelp.UpdatePrunePoint(ctx, rollup, mon.Core); err != nil {
					logger.Error().Err(err).Msg("error pruning database")
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	var stakerDone chan bool
	if stakerManager != nil {
		stakerDone = stakerManager.RunInBackground(ctx, config.Validator.StakerDelay)
	} else {
		stakerDone = make(chan bool)
	}

	select {
	case err := <-txDBErrChan:
		return err
	case err := <-broadcasterErrChan:
		return err
	case err := <-errChan:
		return err
	case err := <-broadcastClientErrChan:
		return err
	case <-stakerDone:
		return nil
	case <-inboxReaderDone:
		return nil
	case <-cancelChan:
		return nil
	}
}

func checkBlockHash(ctx context.Context, clnt *ethclient.Client, db *txdb.TxDB) (bool, error) {
	if clnt == nil {
		return false, errors.New("need a client to check block hash")
	}
	blockCount, err := db.BlockCount()
	if err != nil {
		return false, err
	}
	if blockCount < blockCheckCountDelay {
		return true, nil
	}
	// Use a small block delay here in case the upstream node isn't full caught up
	block, err := db.GetBlock(blockCount - blockCheckCountDelay)
	if err != nil {
		return false, err
	}
	remoteHeader, err := clnt.HeaderByNumber(ctx, block.Header.Number)
	if err != nil {
		return false, err
	}
	if remoteHeader.Hash() == block.Header.Hash() {
		return true, nil
	}
	logger.Warn().
		Str("remote", remoteHeader.Hash().Hex()).
		Str("local", block.Header.Hash().Hex()).
		Msg("mismatched block header")
	return false, nil
}

type ChainState struct {
	ValidatorWallet string `json:"validatorWallet"`
}

func startValidator(
	ctx context.Context,
	config *configuration.Config,
	walletConfig *configuration.Wallet,
	l1Client ethutils.EthClient,
	auth *bind.TransactOpts,
	mon *monitor.Monitor,
) (*staker.Staker, error) {
	if len(config.Validator.UtilsAddress) == 0 ||
		len(config.Validator.WalletFactoryAddress) == 0 || config.Validator.Strategy() == configuration.UnknownStrategy {
		return nil, errors.New("Contract addresses and strategy required for validator")
	}

	rollupAddr := ethcommon.HexToAddress(config.Rollup.Address)
	validatorUtilsAddr := ethcommon.HexToAddress(config.Validator.UtilsAddress)
	validatorWalletFactoryAddr := ethcommon.HexToAddress(config.Validator.WalletFactoryAddress)

	chainState := ChainState{}
	if config.Validator.ContractWalletAddress != "" {
		if !ethcommon.IsHexAddress(config.Validator.ContractWalletAddress) {
			logger.Error().Str("address", config.Validator.ContractWalletAddress).Msg("invalid validator smart contract wallet")
			return nil, errors.New("invalid validator smart contract wallet address")
		}
		chainState.ValidatorWallet = config.Validator.ContractWalletAddress
	} else {
		chainStateFile, err := os.Open(config.Validator.ContractWalletAddressFilename)
		if err != nil {
			// If file doesn't exist yet, will be created when needed
			if !os.IsNotExist(err) {
				return nil, errors.Wrap(err, "failed to open chainState file: "+config.Validator.ContractWalletAddressFilename)
			}
		} else {
			chainStateData, err := ioutil.ReadAll(chainStateFile)
			if err != nil {
				return nil, errors.Wrap(err, "failed to read chain state")
			}
			err = json.Unmarshal(chainStateData, &chainState)
			if err != nil {
				return nil, errors.Wrap(err, "failed to unmarshal chain state")
			}
		}
	}

	var valAuth transactauth.TransactAuth
	var err error
	if len(walletConfig.Fireblocks.SSLKey) > 0 {
		valAuth, _, err = transactauth.NewFireblocksTransactAuthAdvanced(ctx, l1Client, auth, walletConfig, false)
	} else {
		valAuth, err = transactauth.NewTransactAuthAdvanced(ctx, l1Client, auth, false)
	}
	if err != nil {
		return nil, errors.Wrap(err, "error creating wallet auth")
	}
	var validatorAddress *ethcommon.Address
	if chainState.ValidatorWallet != "" {
		logger.Info().Str("address", chainState.ValidatorWallet).Msg("validator using smart contract wallet")
		addr := ethcommon.HexToAddress(chainState.ValidatorWallet)
		validatorAddress = &addr

		valWallet, err := ethbridgecontracts.NewValidator(addr, l1Client)
		if err != nil {
			return nil, err
		}
		owner, err := valWallet.Owner(&bind.CallOpts{Context: ctx})
		if err != nil {
			return nil, err
		}
		if owner != valAuth.From() {
			return nil, fmt.Errorf("validator smart contract wallet owner %v doesn't match validator wallet %v", owner, valAuth.From())
		}
	} else if config.Validator.OnlyCreateWalletContract {
		logger.Info().Msg("only creating validator smart contract and exiting")
	} else {
		return nil, errors.New("validator smart contract wallet not present, add --validator.only-create-wallet-contract to create")
	}

	onValidatorWalletCreated := func(addr ethcommon.Address) {}
	if config.Validator.ContractWalletAddress == "" {
		onValidatorWalletCreated = func(addr ethcommon.Address) {
			chainState.ValidatorWallet = addr.String()
			newChainStateData, err := json.Marshal(chainState)
			if err != nil {
				logger.Warn().Err(err).Msg("failed to marshal chain state")
			} else if err := ioutil.WriteFile(config.Validator.ContractWalletAddressFilename, newChainStateData, 0644); err != nil {
				logger.Warn().Err(err).Msg("failed to write chain state config")
			}
			logger.
				Info().
				Str("address", chainState.ValidatorWallet).
				Str("filename", config.Validator.ContractWalletAddressFilename).
				Msg("created validator smart contract wallet")
		}
	} else {
		onValidatorWalletCreated = func(addr ethcommon.Address) {
			logger.Error().Str("address", addr.String()).Msg("created wallet when --validator.wallet-address provided")
		}
	}

	val, err := ethbridge.NewValidator(validatorAddress, validatorWalletFactoryAddr, rollupAddr, l1Client, valAuth, config.Rollup.FromBlock, config.Rollup.BlockSearchSize, onValidatorWalletCreated)
	if err != nil {
		return nil, errors.Wrap(err, "error creating validator")
	}

	if config.Validator.OnlyCreateWalletContract {
		// Create validator smart contract wallet if needed then exit
		oldValidatorWallet := chainState.ValidatorWallet
		err = val.CreateWalletIfNeeded(ctx)
		if err != nil {
			return nil, err
		}

		if oldValidatorWallet == chainState.ValidatorWallet {
			return nil, errors.Errorf("validator smart contract wallet (%v) already exists, remove --validator.only-create-wallet-contract to run normally", chainState.ValidatorWallet)
		}
		return nil, errors.Errorf("validator smart contract wallet (%v) created, remove --validator.only-create-wallet-contract to run normally", chainState.ValidatorWallet)
	}

	stakerManager, _, err := staker.NewStaker(ctx, mon.Core, l1Client, val, config.Rollup.FromBlock, common.NewAddressFromEth(validatorUtilsAddr), config.Validator.Strategy(), bind.CallOpts{}, valAuth, config.Validator)
	if err != nil {
		return nil, errors.Wrap(err, "error setting up staker")
	}

	logger.Info().Str("strategy", config.Validator.StrategyImpl).Msg("Initialized validator")
	return stakerManager, nil
}
