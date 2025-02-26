package operator

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"strconv"
	"time"

	sdkTypes "github.com/Layr-Labs/eigensdk-go/types"
	"github.com/automata-network/multi-prover-avs/aggregator"
	"github.com/automata-network/multi-prover-avs/contracts/bindings/TEELivenessVerifier"
	"github.com/automata-network/multi-prover-avs/utils"
	"github.com/chzyer/logex"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

type Operator struct {
	cfg    *ConfigContext
	logger *logex.Logger

	operatorAddress common.Address

	aggregator *aggregator.Client

	operatorId    [32]byte
	metrics       *Metrics
	quorumNumbers []byte

	proverClient *ProverClient
	taskFetcher  *LogTracer
	offset       *os.File

	TEELivenessVerifier *TEELivenessVerifier.TEELivenessVerifier
}

func NewOperator(path string) (*Operator, error) {
	cfg, err := ParseConfigContext(path, nil)
	if err != nil {
		return nil, logex.Trace(err)
	}

	proverClient, err := NewProverClient(cfg.Config.ProverURL)
	if err != nil {
		return nil, logex.Trace(err)
	}

	logger := logex.NewLoggerEx(os.Stderr)

	TEELivenessVerifier, err := TEELivenessVerifier.NewTEELivenessVerifier(cfg.Config.TEELivenessVerifierAddress, cfg.AttestationClient)
	if err != nil {
		return nil, logex.Trace(err)
	}

	aggClient, err := aggregator.NewClient(cfg.Config.AggregatorURL)
	if err != nil {
		return nil, logex.Trace(err, "aggregatorURL:"+cfg.Config.AggregatorURL)
	}

	operatorAddress, err := cfg.QueryOperatorAddress()
	if err != nil {
		return nil, logex.Trace(err, "queryOperatorAddr")
	}

	if operatorAddress == utils.ZeroAddress {
		return nil, logex.NewErrorf("operator is not registered")
	}

	quorumNames := map[sdkTypes.QuorumNum]string{
		0: "Scroll SGX Quorum",
	}
	quorumNumbers := []byte{0}

	metrics := NewMetrics(cfg.EigenClients, utils.NewLogger(logger), operatorAddress, cfg.Config.EigenMetricsIpPortAddress, quorumNames)

	operator := &Operator{
		cfg:                 cfg,
		proverClient:        proverClient,
		logger:              logger,
		quorumNumbers:       quorumNumbers,
		aggregator:          aggClient,
		operatorAddress:     operatorAddress,
		metrics:             metrics,
		TEELivenessVerifier: TEELivenessVerifier,
	}

	if cfg.Config.TaskFetcher != nil {
		taskFetcherClient, err := ethclient.Dial(cfg.Config.TaskFetcher.Endpoint)
		if err != nil {
			return nil, logex.Trace(err)
		}

		offsetFile, err := os.OpenFile(cfg.Config.TaskFetcher.OffsetFile, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return nil, logex.Trace(err)
		}

		operator.offset = offsetFile
		operator.taskFetcher = NewLogTracer(taskFetcherClient, &LogTracerConfig{
			Id:               "operator-log-tracer",
			Wait:             5,
			Max:              100,
			ScanIntervalSecs: cfg.Config.TaskFetcher.ScanIntervalSecs,
			Topics:           cfg.Config.TaskFetcher.Topics,
			Addresses:        cfg.Config.TaskFetcher.Addresses,
			Handler:          operator,
			SkipOnError:      true,
		})
	}

	return operator, nil
}

// callback func for task fetcher
func (h *Operator) GetBlock() (uint64, error) {
	data := make([]byte, 16)
	n, err := h.offset.ReadAt(data, 0)
	if n == 0 {
		if err == io.EOF {
			return 0, nil
		}
		return 0, logex.Trace(err, n)
	}
	data = bytes.Trim(data[:n], "\x00\r\n ")

	number, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return 0, logex.Trace(err)
	}
	return uint64(number), nil
}

// callback func for task fetcher
func (h *Operator) SaveBlock(offset uint64) error {
	data := []byte(strconv.FormatUint(offset, 10))
	_, err := h.offset.WriteAt(data, 0)
	return err
}

// callback func for task fetcher
func (o *Operator) OnNewLog(ctx context.Context, log *types.Log) error {
	blockHeader, err := o.cfg.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		return logex.Trace(err)
	}

	// parse the task
	poe, skip, err := o.proverGetPoe(ctx, log.TxHash, log.Topics)
	if err != nil {
		return logex.Trace(err)
	}
	if skip {
		return nil
	}

	md := &aggregator.Metadata{
		BatchId:    poe.BatchId,
		StartBlock: poe.StartBlock,
		EndBlock:   poe.EndBlock,
	}
	mdBytes, err := json.Marshal(md)
	if err != nil {
		return logex.Trace(err)
	}

	stateHeader := &aggregator.StateHeader{
		Identifier:                 (*hexutil.Big)(big.NewInt(o.cfg.Config.Identifier)),
		Metadata:                   mdBytes,
		State:                      poe.Poe.Pack(),
		QuorumNumbers:              o.quorumNumbers,
		QuorumThresholdPercentages: []byte{0},
		ReferenceBlockNumber:       uint32(blockHeader.Number.Int64() - 1),
	}

	logex.Pretty(stateHeader)

	digest, err := stateHeader.Digest()
	if err != nil {
		return logex.Trace(err)
	}
	sig := o.cfg.BlsKey.SignMessage(digest)

	// submit to aggregator
	if err := o.aggregator.SubmitTask(ctx, &aggregator.TaskRequest{
		Task:       stateHeader,
		Signature:  sig,
		OperatorId: o.operatorId,
	}); err != nil {
		return logex.Trace(err)
	}

	return nil
}

func (o *Operator) Start(ctx context.Context) error {
	logex.Info("starting operator...")
	isSimulation, err := o.TEELivenessVerifier.Simulation(nil)
	if err != nil {
		return logex.Trace(err, "TEE")
	}
	if isSimulation != o.cfg.Config.Simulation {
		return logex.NewErrorf("simulation mode not match with the contract: local:%v, remote:%v", o.cfg.Config.Simulation, isSimulation)
	}
	if err := o.checkIsRegistered(); err != nil {
		return logex.Trace(err)
	}
	if err := o.RegisterAttestationReport(ctx); err != nil {
		return logex.Trace(err, utils.EcdsaAddress(o.cfg.AttestationEcdsaKey))
	}
	errChan := o.metrics.Start(ctx)
	go func() {
		err := <-errChan
		logex.Fatal(err)
	}()

	o.logger.Infof("Started Operator... operator info: operatorId=%v, operatorAddr=%v, operatorG1Pubkey=%v, operatorG2Pubkey=%v",
		hex.EncodeToString(o.operatorId[:]),
		o.operatorAddress,
		o.cfg.BlsKey.GetPubKeyG1(),
		o.cfg.BlsKey.GetPubKeyG2(),
	)

	if err := o.taskFetcher.Run(ctx); err != nil {
		return logex.Trace(err)
	}

	return nil
}

func (o *Operator) checkIsRegistered() error {
	operatorIsRegistered, err := o.cfg.EigenClients.AvsRegistryChainReader.IsOperatorRegistered(nil, o.operatorAddress)
	if err != nil {
		return logex.Trace(err)
	}
	if !operatorIsRegistered {
		return logex.NewErrorf("operator[%v] is not registered", o.operatorAddress)
	}
	o.operatorId, err = o.cfg.EigenClients.AvsRegistryChainReader.GetOperatorId(nil, o.operatorAddress)
	if err != nil {
		return logex.Trace(err)
	}
	return nil
}

var ABI = func() abi.ABI {
	ty := `[{"inputs":[{"internalType":"uint8","name":"_version","type":"uint8"},{"internalType":"bytes","name":"_parentBatchHeader","type":"bytes"},{"internalType":"bytes[]","name":"_chunks","type":"bytes[]"},{"internalType":"bytes","name":"_skippedL1MessageBitmap","type":"bytes"}],"name":"commitBatch","outputs":[],"stateMutability":"nonpayable","type":"function"}]`
	result, err := abi.JSON(bytes.NewReader([]byte(ty)))
	if err != nil {
		panic(err)
	}
	return result
}()

func (o *Operator) proverGetPoe(ctx context.Context, txHash common.Hash, topics []common.Hash) (*PoeResponse, bool, error) {
	if o.cfg.Config.Simulation {
		tx, _, err := o.taskFetcher.source.TransactionByHash(ctx, txHash)
		if err != nil {
			return nil, false, logex.Trace(err)
		}
		args, err := ABI.Methods["commitBatch"].Inputs.Unpack(tx.Data()[4:])
		if err != nil {
			return nil, false, logex.Trace(err)
		}

		startBlock := int64(0)
		endBlock := int64(0)
		for _, chunk := range args[2].([][]byte) {
			for i := 0; i < int(chunk[0]); i++ {
				blockNumber := int64(binary.BigEndian.Uint64(chunk[1:][i*60 : i*60+8]))
				if startBlock == 0 {
					startBlock = blockNumber
				} else {
					endBlock = blockNumber
				}
			}
		}

		startBlockHeader, err := o.taskFetcher.source.HeaderByNumber(ctx, big.NewInt(startBlock))
		if err != nil {
			return nil, false, logex.Trace(err)
		}
		endBlockHeader, err := o.taskFetcher.source.HeaderByNumber(ctx, big.NewInt(endBlock))
		if err != nil {
			return nil, false, logex.Trace(err)
		}

		response := &PoeResponse{
			Poe: &Poe{
				BatchHash:     topics[2],
				NewStateRoot:  endBlockHeader.Root,
				PrevStateRoot: startBlockHeader.Root,
			},
			StartBlock: uint64(startBlock),
			EndBlock:   uint64(endBlock),
		}
		return response, false, nil
	}

	logex.Infof("fetching poe for batch %v", topics[2])
	poe, skip, err := o.proverClient.GetPoe(ctx, txHash)
	if err != nil {
		return nil, skip, logex.Trace(err)
	}
	return poe, skip, nil
}

func (o *Operator) proverGetAttestationReport(ctx context.Context, pubkey []byte) ([]byte, error) {
	if o.cfg.Config.Simulation {
		quote, err := generateSimulationQuote(pubkey)
		if err != nil {
			return nil, logex.Trace(err)
		}
		return quote, nil
	}
	quote, err := o.proverClient.GenerateAttestaionReport(ctx, pubkey)
	if err != nil {
		return nil, logex.Trace(err)
	}
	return quote, nil
}

func (o *Operator) registerAttestationReport(ctx context.Context, pubkeyBytes []byte) error {
	report, err := o.proverGetAttestationReport(ctx, pubkeyBytes)
	if err != nil {
		return logex.Trace(err)
	}
	chainId, err := o.cfg.AttestationClient.ChainID(ctx)
	if err != nil {
		return logex.Trace(err)
	}
	opt, err := bind.NewKeyedTransactorWithChainID(o.cfg.AttestationEcdsaKey, chainId)
	if err != nil {
		return logex.Trace(err)
	}

	tx, err := o.TEELivenessVerifier.SubmitLivenessProof(opt, report)
	if err != nil {
		return logex.Trace(err)
	}
	logex.Infof("submitted liveness proof: %v", tx.Hash())
	if _, err := utils.WaitTx(ctx, o.cfg.AttestationClient, tx, nil); err != nil {
		return logex.Trace(err)
	}
	logex.Infof("registered in TEELivenessVerifier: %v", tx.Hash())
	return nil
}

func (o *Operator) RegisterAttestationReport(ctx context.Context) error {
	logex.Info("checking tee liveness...")
	pubkeyBytes := o.cfg.BlsKey.PubKey.Serialize()
	if len(pubkeyBytes) != 64 {
		return logex.NewErrorf("invalid pubkey")
	}

	var x, y [32]byte
	copy(x[:], pubkeyBytes[:32])
	copy(y[:], pubkeyBytes[32:64])
	isRegistered, err := o.TEELivenessVerifier.VerifyLivenessProof(nil, x, y)
	if err != nil {
		return logex.Trace(err)
	}
	if isRegistered {
		logex.Info("Operater has registered on TEE Liveness Verifier")
	} else {
		if err := o.registerAttestationReport(ctx, pubkeyBytes); err != nil {
			return logex.Trace(err)
		}
	}

	checkNext := func(ctx context.Context) error {
		validSecs, err := o.TEELivenessVerifier.AttestValiditySeconds(nil)
		if err != nil {
			return logex.Trace(err)
		}
		key := crypto.Keccak256Hash(pubkeyBytes)
		prover, err := o.TEELivenessVerifier.AttestedProvers(nil, key)
		if err != nil {
			return logex.Trace(err)
		}
		deadline := prover.Time.Int64() + validSecs.Int64()
		now := time.Now().Unix()
		logex.Info("next attestation will be at", time.Unix(deadline, 0))
		if deadline > now+300 {
			time.Sleep(time.Duration(deadline-now-300) * time.Second)
		}
		return o.registerAttestationReport(ctx, pubkeyBytes)
	}
	go func() {
		ctx := context.Background()
		for {
			if err := checkNext(ctx); err != nil {
				logex.Error(err)
			}
		}
	}()

	return nil
}
