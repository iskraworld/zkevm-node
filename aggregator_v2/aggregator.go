package aggregator2

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/aggregator_v2/pb"
	"github.com/0xPolygonHermez/zkevm-node/aggregator_v2/prover"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/iden3/go-iden3-crypto/keccak256"
	"github.com/jackc/pgx/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

type proverJob struct {
	jobType string           // TODO make enum
	jobChan chan interface{} // TODO make job result type
}

type proverCli struct {
	ctx     context.Context
	jobChan chan proverJob
}

// AggregatorV2 represents an aggregator
type AggregatorV2 struct {
	pb.UnimplementedAggregatorServiceServer

	cfg Config

	State                stateInterface
	EthTxManager         ethTxManager
	Ethman               etherman
	ProfitabilityChecker aggregatorTxProfitabilityChecker

	timeSendFinalProof time.Time
	mu                 *sync.Mutex
	provers            chan proverCli
	cutOff             <-chan time.Time
	finalCh            chan proverJob // this needs to be passed on prover init
	srv                *grpc.Server
	ctx                context.Context
	exit               context.CancelFunc
}

// New creates a new aggregator.
func New(
	cfg Config,
	stateInterface stateInterface,
	ethTxManager ethTxManager,
	etherman etherman,
) (*AggregatorV2, error) {
	var profitabilityChecker aggregatorTxProfitabilityChecker // FIXME
	switch cfg.TxProfitabilityCheckerType {
	case ProfitabilityBase:
		profitabilityChecker = NewTxProfitabilityCheckerBase(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration, cfg.TxProfitabilityMinReward.Int)
	case ProfitabilityAcceptAll:
		profitabilityChecker = NewTxProfitabilityCheckerAcceptAll(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration)
	}

	a := &AggregatorV2{
		State:                stateInterface,
		EthTxManager:         ethTxManager,
		Ethman:               etherman,
		ProfitabilityChecker: profitabilityChecker,
		cfg:                  cfg,
		mu:                   &sync.Mutex{},
	}

	return a, nil
}

// Start starts the aggregator
func (a *AggregatorV2) Start(ctx context.Context) {
	var cancel context.CancelFunc
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.exit = cancel
	a.timeSendFinalProof = time.Now().Add(a.cfg.IntervalToSendFinalProof.Duration)

	a.cutOff = time.After(a.cfg.IntervalToSendFinalProof.Duration)
	a.provers = make(chan proverCli)
	a.finalCh = make(chan proverJob)

	address := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	a.srv = grpc.NewServer()
	pb.RegisterAggregatorServiceServer(a.srv, a)

	healthService := newHealthChecker()
	grpc_health_v1.RegisterHealthServer(a.srv, healthService)

	go func() {
		log.Infof("Server listening on port %d", a.cfg.Port)
		if err := a.srv.Serve(lis); err != nil {
			a.exit()
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	// Wait until context is done
	<-ctx.Done()
	a.Stop()
}

// Stop stops the Aggregator server.
func (a *AggregatorV2) Stop() {
	a.srv.Stop()
	a.exit()
}

func (a *AggregatorV2) orchestrate() {
	proofChan := make(chan interface{})
	finalChan := make(chan interface{})
	for {
		select {
		case <-a.ctx.Done():
			return
		case final := <-finalChan:
			_ = final
			// TODO send final
			a.cutOff = time.After(a.cfg.IntervalToSendFinalProof.Duration)
		case proof := <-proofChan:
			select {
			case <-a.cutOff:
				select {
				case <-a.ctx.Done():
					return
				default:
					a.finalCh <- proverJob{
						jobType: "final",
						jobChan: finalChan,
					}
				}
			default:
				_ = proof
				// TODO store proof
			}
		case prover := <-a.provers:
			select {
			case <-a.ctx.Done():
				return
			case <-prover.ctx.Done():
				continue
			default:
				// check aggregate
				if true {
					prover.jobChan <- proverJob{
						jobType: "aggregate",
						jobChan: proofChan,
					}
				} else {
					// check batches
					prover.jobChan <- proverJob{
						jobType: "generate",
						jobChan: proofChan,
					}
				}
			}
		}
	}
}

// Channel implements the bi-directional communication channel between the
// Prover client and the Aggregator server.
func (a *AggregatorV2) Channel(stream pb.AggregatorService_ChannelServer) error {
	prover, err := prover.New(&a.cfg.Prover, stream)
	if err != nil {
		return err
	}
	log.Debugf("establishing stream connection for prover %s", prover.ID())

	ctx := stream.Context()

	go func() {
		for {
			select {
			case <-a.ctx.Done():
				// server disconnected
				return
			case <-ctx.Done():
				// client disconnected
				return
			case <-a.cutOff:
				err := a.sendFinalProof(ctx, prover)
				if err != nil {
					log.Errorf("failed to send final proof: ", err)
					a.exit()
					return
				}
				a.cutOff = time.After(a.cfg.IntervalToSendFinalProof.Duration)
			default:
				if !prover.IsIdle() {
					log.Warn("Prover ID %s is not idle", prover.ID())
					time.Sleep(a.cfg.IntervalToConsolidateState.Duration)
					continue
				}

				// try to aggregate first
				proofGenerated, err := a.aggregateProofs(ctx, prover)
				if err != nil {
					log.Errorf("failed to aggregate proofs: %w", err)
					a.exit()
					return
				}
				if !proofGenerated {
					// nothing to aggregate, try to generate a proof from a batch
					proofGenerated, err = a.generateProof(ctx, prover)
					if err != nil {
						log.Errorf("failed to generate batch proof: %w", err)
						a.exit()
						return
					}
				}
				if !proofGenerated {
					// if proof was generated we retry immediatly as probably we have more proofs to process
					// if no proof was generated (aggregated or batch) wait some time waiting before retry
					time.Sleep(a.cfg.IntervalToConsolidateState.Duration)
				}
			}
		}
	}()

	// keep this scope alive, the stream gets closed if we exit from here.
	for {
		select {
		case <-a.ctx.Done():
			// server disconnecting
			return nil
		case <-ctx.Done():
			// client disconnected
			// TODO(pg): reconnect?
			return nil
		}
	}

}

func (a *AggregatorV2) getAndLockProofToFinalize(ctx context.Context) (*state.RecursiveProof, error) {
	// TODO
	return nil, nil
}

func (a *AggregatorV2) sendFinalProof(ctx context.Context, prover proverInterface) error {
	log.Debug("Checking if network is synced")
	for !a.isSynced(ctx) {
		log.Debug("Waiting for synchronizer to sync...")
		continue
	}

	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if errors.Is(err, state.ErrNotFound) {
		// no verified batches, swallow the error
		log.Debug("Found no verified batches")
		return nil
	}
	if err != nil {
		return fmt.Errorf("Failed to get last verified batch: %w", err)
	}

	batchNumberToVerify := lastVerifiedBatch.BatchNumber + 1

	proof, err := a.getAndLockProofToFinalize(ctx)
	if errors.Is(err, state.ErrNotFound) {
		// no to finalize, swallow the error
		log.Debug("Found no proof to finalize")
		return nil
	}
	if err != nil {
		return fmt.Errorf("Failed to retrieve proof to finalize: %w", err)
	}

	if proof.BatchNumber != batchNumberToVerify {
		log.Infof("Proof batch number %d is not the following to last verfied batch number %d", proof.BatchNumber, batchNumberToVerify)
		return nil
	}

	bComplete, err := a.State.CheckProofContainsCompleteSequences(ctx, proof, nil)

	if !bComplete {
		log.Infof("Recursive proof %d-%d does not contain completes sequences", proof.BatchNumber, proof.BatchNumberFinal)
		return nil
	}

	log.Infof("Prover %s is going to be used to generate final proof for batches: %d-%d", prover.ID(), proof.BatchNumber, proof.BatchNumberFinal)

	finalProofID, err := prover.FinalProof(proof.Proof)
	if err != nil {
		return fmt.Errorf("Failed to instruct prover to prepare final proof: %w", err)
	}

	finalProof, err := prover.WaitFinalProof(ctx, finalProofID)
	if err != nil {
		return fmt.Errorf("Failed to get final proof: %w", err)
	}

	//b, err := json.Marshal(resGetProof.FinalProof)
	log.Infof("Final proof %s generated", *proof.ProofID)

	var inputProver *pb.InputProver
	json.Unmarshal([]byte(proof.InputProver), inputProver)
	a.compareInputHashes(inputProver, finalProof)

	// Handle local exit root in the case of the mock prover
	if string(finalProof.Public.NewLocalExitRoot[:]) == "0x17c04c3760510b48c6012742c540a81aba4bca2f78b9d14bfd2f123e2e53ea3e" {
		// This local exit root comes from the mock, use the one captured by the executor instead
		log.Warnf("NewLocalExitRoot looks like a mock value")
		/*log.Warnf(
			"NewLocalExitRoot looks like a mock value, using value from executor instead: %v",
			proof.InputProver.PublicInputs.NewLocalExitRoot,
		)*/
		//resGetProof.Public.PublicInputs.NewLocalExitRoot = proof.InputProver.PublicInputs.NewLocalExitRoot
	}

	log.Infof("Verfiying final proof with ethereum smart contract, batches %d-%d", proof.BatchNumber, proof.BatchNumberFinal)
	// · Not working with mock prover _, err = a.Ethman.VerifyBatches2(ctx, proof.BatchNumber-1, proof.BatchNumberFinal, resGetProof, 0, nil, nil)
	if err != nil {
		log.Errorf("Error verifiying final proof for batches %d-%d, err: %w", proof.BatchNumber, proof.BatchNumberFinal, err)
		return err
	}

	/* · Is needed to do this additional steps??
	err = c.state.UpdateProofTx(ctx, pendingProof.BatchNumber, tx.Hash(), tx.Nonce(), nil)
	if err != nil {
		log.Errorf("failed to update tx to verify batch for batch number %v, new tx hash %v, nonce %v, err: %v",
			pendingProof.BatchNumber, tx.Hash().String(), tx.Nonce(), err)
		break
	}
	err = c.ethMan.WaitTxToBeMined(ctx, tx, c.cfg.IntervalToReviewVerifyBatchTx.Duration)
	if err != nil {
		log.Errorf("error waiting tx to be mined: %s, error: %w", tx.Hash(), err)
		break
	}
	txHash := tx.Hash()
	pendingProof.TxHash = &txHash
	nonce := tx.Nonce()
	pendingProof.TxNonce = &nonce
	time.Sleep(time.Second * 2) // nolint
	*/

	log.Infof("Final proof for batches %d-%d verified", proof.BatchNumber, proof.BatchNumberFinal)

	return nil
}

func (a *AggregatorV2) unlockProofsToAggregate(ctx context.Context, proof1, proof2 *state.RecursiveProof) error {
	// Release proofs from aggregating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Warnf("Failed to begin transaction to release proof aggregation state, err: %v", err)
		return err
	}

	proof1.Generating = false
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof1, dbTx)
	if err != nil {
		log.Warnf("Failed to release proof aggregation state, err: %v", err)
		dbTx.Rollback(ctx)
		return err
	}

	proof2.Generating = false
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof2, dbTx)
	if err != nil {
		log.Warnf("Failed to release proof aggregation state, err: %v", err)
		dbTx.Rollback(ctx)
		return err
	}

	if err := dbTx.Commit(ctx); err != nil {
		return err
	}

	return nil
}

func (a *AggregatorV2) getAndLockProofsToAggregate(ctx context.Context) (*state.RecursiveProof, *state.RecursiveProof, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Set proofs in aggregating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("Failed to begin transaction to set proof aggregation state, err: %v", err)
		return nil, nil, err
	}

	proof1, proof2, err := a.State.GetRecursiveProofsToAggregate(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	proof1.Generating = true
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof1, dbTx)
	if err != nil {
		log.Errorf("Failed to set proof aggregation state, err: %v", err)
		dbTx.Rollback(ctx)
		return nil, nil, err
	}

	proof2.Generating = true
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof2, dbTx)
	if err != nil {
		log.Errorf("Failed to set proof aggregation state, err: %v", err)
		dbTx.Rollback(ctx)
		return nil, nil, err
	}

	if err := dbTx.Commit(ctx); err != nil {
		return nil, nil, err
	}

	return proof1, proof2, nil
}

func (a *AggregatorV2) aggregateProofs(ctx context.Context, prover proverInterface) (ok bool, err error) {
	log.Debugf("aggregateProofs start %s", prover.ID())

	var proof1, proof2 *state.RecursiveProof

	proof1, proof2, err = a.getAndLockProofsToAggregate(ctx)
	if errors.Is(err, state.ErrNotFound) {
		// nothing to aggregate, swallow the error
		log.Debug("Nothing to aggregate")
		return false, nil
	}
	if err != nil {
		return false, err
	}

	defer func() {
		if err != nil {
			err2 := a.unlockProofsToAggregate(ctx, proof1, proof2)
			if err2 != nil {
				log.Errorf("Failed to release aggregated proofs, err: %v", err2)
			}
		}
	}()

	log.Infof("Prover %s is going to be used to aggregate proofs: %d-%d and %d-%d",
		prover.ID(), proof1.BatchNumber, proof1.BatchNumberFinal, proof2.BatchNumber, proof2.BatchNumberFinal)

	proverID := prover.ID()

	aggrProof := &state.RecursiveProof{
		BatchNumber:      proof1.BatchNumber,
		BatchNumberFinal: proof2.BatchNumberFinal,
		Prover:           &proverID,
		InputProver:      proof1.InputProver,
		Generating:       false,
	}

	var aggrProofID string
	aggrProofID, err = prover.AggregatedProof(proof1.Proof, proof2.Proof)
	if err != nil {
		log.Errorf("Failed to instruct prover to generate aggregated proof: %w", err)
		return false, err
	}

	aggrProof.ProofID = &aggrProofID

	log.Infof("Proof ID for aggregated proof %d-%d: %v", aggrProof.BatchNumber, aggrProof.BatchNumberFinal, *aggrProof.ProofID)

	var proof string
	proof, err = prover.WaitRecursiveProof(ctx, aggrProofID)
	if err != nil {
		log.Errorf("Failed to retrieve aggregated proof from prover: %w", err)
		return false, err
	}

	aggrProof.Proof = proof
	time.Sleep(time.Duration(rand.Intn(20)+10) * time.Second)

	var dbTx pgx.Tx
	dbTx, err = a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("Failed to begin transaction to store proof aggregation result, err: %w", err)
		return false, err
	}

	err = a.State.AddGeneratedRecursiveProof(ctx, aggrProof, dbTx)
	if err != nil {
		dbTx.Rollback(ctx)
		log.Errorf("Failed to store proof aggregation result, err: %v", err)
		return false, err
	}

	// Delete aggregated proofs
	err = a.State.DeleteGeneratedRecursiveProof(ctx, proof1.BatchNumber, proof1.BatchNumberFinal, nil)
	if err != nil {
		dbTx.Rollback(ctx)
		log.Errorf("Failed to delete aggregation input proof 1: %w", err)
		return false, err
	}
	err = a.State.DeleteGeneratedRecursiveProof(ctx, proof2.BatchNumber, proof2.BatchNumberFinal, nil)
	if err != nil {
		dbTx.Rollback(ctx)
		log.Errorf("Failed to delete aggregation input proof 2: %w", err)
		return false, err
	}

	dbTx.Commit(ctx)

	log.Debug("aggregateProofs end")

	return true, nil
}

func (a *AggregatorV2) getAndLockBatchToProve(ctx context.Context, prover proverInterface) (*state.Batch, *state.RecursiveProof, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Get virtual batch pending to generate proof
	batchToVerify, err := a.State.GetVirtualBatchToRecursiveProve(ctx, lastVerifiedBatch.BatchNumber, nil)
	if err != nil {
		return nil, nil, err
	}

	log.Infof("Found virtual batch %d pending to generate proof", batchToVerify.BatchNumber)

	log.Infof("Checking profitability to aggregate batch, batchNumber: %d", batchToVerify.BatchNumber)
	// pass matic collateral as zero here, bcs in smart contract fee for aggregator is not defined yet
	isProfitable, err := a.ProfitabilityChecker.IsProfitable(ctx, big.NewInt(0))
	if err != nil {
		log.Errorf("Failed to check aggregator profitability, err: %v", err)
		return nil, nil, err
	}

	if !isProfitable {
		log.Infof("Batch %d is not profitable, matic collateral %d", batchToVerify.BatchNumber, big.NewInt(0))
		return nil, nil, err
	}

	proverID := prover.ID()
	proof := &state.RecursiveProof{
		BatchNumber:      batchToVerify.BatchNumber,
		BatchNumberFinal: batchToVerify.BatchNumber,
		Prover:           &proverID,
		Generating:       false,
	}

	// Avoid other thread to process the same batch
	err = a.State.AddGeneratedRecursiveProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to add batch proof, err: %v", err)
		return nil, nil, err
	}

	return batchToVerify, proof, nil
}

func (a *AggregatorV2) generateProof(ctx context.Context, prover proverInterface) (ok bool, err error) {
	var (
		batchToProve *state.Batch
		proof        *state.RecursiveProof
	)
	batchToProve, proof, err = a.getAndLockBatchToProve(ctx, prover)
	if errors.Is(err, state.ErrNotFound) {
		// nothing to prove, swallow the error
		log.Debug("No batches to prove")
		return false, nil
	}
	if err != nil {
		return false, err
	}

	defer func() {
		if err != nil {
			err2 := a.State.DeleteGeneratedRecursiveProof(ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err2 != nil {
				log.Errorf("Failed to delete proof in progress, err: %v", err2)
			}
		}
	}()

	log.Infof("Prover %s is going to be used for batchNumber: %d", prover.ID(), batchToProve.BatchNumber)

	log.Infof("Sending zki + batch to the prover, batchNumber: %d", batchToProve.BatchNumber)
	inputProver, err := a.buildInputProver(ctx, batchToProve)
	if err != nil {
		log.Errorf("Failed to build input prover, err: %v", err)
		return false, err
	}

	b, err := json.Marshal(inputProver)
	if err != nil {
		return false, fmt.Errorf("Failed serialize input prover, err: %w", err)
	}
	proof.InputProver = string(b)

	log.Infof("Sending a batch to the prover, OLDSTATEROOT: %s, OLDBATCHNUM: %d",
		inputProver.PublicInputs.OldStateRoot, inputProver.PublicInputs.OldBatchNum)

	genProofID, err := prover.BatchProof(inputProver)
	if err != nil {
		return false, fmt.Errorf("Failed instruct prover to prove a batch: %w", err)
	}

	proof.ProofID = &genProofID

	log.Infof("Proof ID for batchNumber %d: %v", proof.BatchNumber, *proof.ProofID)

	resGetProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
	if err != nil {
		log.Errorf("Failed to get proof from prover, err: %v", err)
		return false, err
	}

	log.Infof("Batch proof %s generated", *proof.ProofID)

	proof.Proof = resGetProof
	proof.Generating = false

	// TODO(pg): remove?
	// a.compareInputHashes(proof.InputProver, proof.Proof)

	// TODO(pg): remove?
	// Handle local exit root in the case of the mock prover
	// if proof.Proof.Public.PublicInputs.NewLocalExitRoot == "0x17c04c3760510b48c6012742c540a81aba4bca2f78b9d14bfd2f123e2e53ea3e" {
	// 	// This local exit root comes from the mock, use the one captured by the executor instead
	// 	log.Warnf(
	// 		"NewLocalExitRoot looks like a mock value, using value from executor instead: %v",
	// 		proof.InputProver.PublicInputs.NewLocalExitRoot,
	// 	)
	// 	resGetProof.Public.PublicInputs.NewLocalExitRoot = proof.InputProver.PublicInputs.NewLocalExitRoot
	// }

	// Store proof
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to store batch proof result, err: %v", err)
		return false, err
	}

	return true, nil
}

func (a *AggregatorV2) isSynced(ctx context.Context) bool {
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil && err != state.ErrNotFound {
		log.Warnf("Failed to get last consolidated batch, err: %v", err)
		return false
	}
	if lastVerifiedBatch == nil {
		return false
	}
	lastVerifiedEthBatchNum, err := a.Ethman.GetLatestVerifiedBatchNum()
	if err != nil {
		log.Warnf("Failed to get last eth batch, err: %v", err)
		return false
	}
	if lastVerifiedBatch.BatchNumber < lastVerifiedEthBatchNum {
		log.Infof("Waiting for the state to be synced, lastVerifiedBatchNum: %d, lastVerifiedEthBatchNum: %d",
			lastVerifiedBatch.BatchNumber, lastVerifiedEthBatchNum)
		return false
	}
	return true
}

func (a *AggregatorV2) buildInputProver(ctx context.Context, batchToVerify *state.Batch) (*pb.InputProver, error) {
	previousBatch, err := a.State.GetBatchByNumber(ctx, batchToVerify.BatchNumber-1, nil)
	if err != nil && err != state.ErrStateNotSynchronized {
		return nil, fmt.Errorf("Failed to get previous batch, err: %v", err)
	}

	blockTimestampByte := make([]byte, 8) //nolint:gomnd
	binary.BigEndian.PutUint64(blockTimestampByte, uint64(batchToVerify.Timestamp.Unix()))
	batchHashData := common.BytesToHash(keccak256.Hash(
		batchToVerify.BatchL2Data,
		batchToVerify.GlobalExitRoot[:],
		blockTimestampByte,
		batchToVerify.Coinbase[:],
	))
	pubAddr, err := a.Ethman.GetPublicAddress()
	if err != nil {
		return nil, fmt.Errorf("failed to get public address, err: %w", err)
	}
	inputProver := &pb.InputProver{
		PublicInputs: &pb.PublicInputs{
			OldStateRoot:    previousBatch.StateRoot.Bytes(),
			OldAccInputHash: []byte(batchHashData.String()), //previousBatch.acc_input_hash
			OldBatchNum:     previousBatch.BatchNumber,
			ChainId:         a.cfg.ChainID,
			BatchL2Data:     batchToVerify.BatchL2Data,
			GlobalExitRoot:  batchToVerify.GlobalExitRoot.Bytes(),
			EthTimestamp:    uint64(batchToVerify.Timestamp.Unix()),
			SequencerAddr:   batchToVerify.Coinbase.String(),
			AggregatorAddr:  pubAddr.String(),
		},
		Db:                map[string]string{},
		ContractsBytecode: map[string]string{},
	}

	return inputProver, nil
}

func (a *AggregatorV2) compareInputHashes(ip *pb.InputProver, resGetProof *pb.FinalProof) {
	/*	// Calc inputHash
		batchNumberByte := make([]byte, 8) //nolint:gomnd
		binary.BigEndian.PutUint64(batchNumberByte, ip.PublicInputs.OldBatchNum)
		blockTimestampByte := make([]byte, 8) //nolint:gomnd
		binary.BigEndian.PutUint64(blockTimestampByte, ip.PublicInputs.EthTimestamp)
		hash := keccak256.Hash(
			[]byte(ip.PublicInputs.OldStateRoot)[:],
			[]byte(ip.PublicInputs.OldLocalExitRoot)[:],
			[]byte(ip.PublicInputs.NewStateRoot)[:],
			[]byte(ip.PublicInputs.NewLocalExitRoot)[:],
			[]byte(ip.PublicInputs.SequencerAddr)[:],
			[]byte(ip.PublicInputs.BatchHashData)[:],
			batchNumberByte[:],
			blockTimestampByte[:],
		)
		// Prime field. It is the prime number used as the order in our elliptic curve
		const fr = "21888242871839275222246405745257275088548364400416034343698204186575808495617"
		frB, _ := new(big.Int).SetString(fr, encoding.Base10)
		inputHashMod := new(big.Int).Mod(new(big.Int).SetBytes(hash), frB)
		internalInputHash := inputHashMod.Bytes()

		// InputHash must match
		internalInputHashS := fmt.Sprintf("0x%064s", hex.EncodeToString(internalInputHash))
		publicInputsExtended := resGetProof.GetPublic()
		if resGetProof.GetPublic().InputHash != internalInputHashS {
			log.Error("inputHash received from the prover (", publicInputsExtended.InputHash,
				") doesn't match with the internal value: ", internalInputHashS)
			log.Debug("internalBatchHashData: ", ip.PublicInputs.BatchHashData, " externalBatchHashData: ", publicInputsExtended.PublicInputs.BatchHashData)
			log.Debug("inputProver.PublicInputs.OldStateRoot: ", ip.PublicInputs.OldStateRoot)
			log.Debug("inputProver.PublicInputs.OldLocalExitRoot:", ip.PublicInputs.OldLocalExitRoot)
			log.Debug("inputProver.PublicInputs.NewStateRoot: ", ip.PublicInputs.NewStateRoot)
			log.Debug("inputProver.PublicInputs.NewLocalExitRoot: ", ip.PublicInputs.NewLocalExitRoot)
			log.Debug("inputProver.PublicInputs.SequencerAddr: ", ip.PublicInputs.SequencerAddr)
			log.Debug("inputProver.PublicInputs.BatchHashData: ", ip.PublicInputs.BatchHashData)
			log.Debug("inputProver.PublicInputs.BatchNum: ", ip.PublicInputs.BatchNum)
			log.Debug("inputProver.PublicInputs.EthTimestamp: ", ip.PublicInputs.EthTimestamp)
		}*/
}

func waitTick(ctx context.Context, ticker *time.Ticker) {
	select {
	case <-ticker.C:
		// nothing
	case <-ctx.Done():
		return
	}
}

// healthChecker will provide an implementation of the HealthCheck interface.
type healthChecker struct{}

// newHealthChecker returns a health checker according to standard package
// grpc.health.v1.
func newHealthChecker() *healthChecker {
	return &healthChecker{}
}

// HealthCheck interface implementation.

// Check returns the current status of the server for unary gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Check(ctx context.Context, req *grpc_health_v1.HealthCheckRequest) (*grpc_health_v1.HealthCheckResponse, error) {
	log.Info("Serving the Check request for health check")
	return &grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_SERVING,
	}, nil
}

// Watch returns the current status of the server for stream gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Watch(req *grpc_health_v1.HealthCheckRequest, server grpc_health_v1.Health_WatchServer) error {
	log.Info("Serving the Watch request for health check")
	return server.Send(&grpc_health_v1.HealthCheckResponse{
		Status: grpc_health_v1.HealthCheckResponse_SERVING,
	})
}
