package aggregator2

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
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

var ErrNotValidForFinal error = errors.New("proof not valid to be sent as final")

type proverJob interface {
	Proof()
}

type jobResult struct {
	proverID string
	job      proverJob
	proof    *state.RecursiveProof
	err      error
}

type aggregationJob struct {
	proof1 *state.RecursiveProof
	proof2 *state.RecursiveProof
}

func (aggregationJob) Proof() {}

type generationJob struct {
	batch *state.Batch
	proof *state.RecursiveProof
}

func (generationJob) Proof() {}

type finalJob struct {
	proof    *state.RecursiveProof
	resultCh chan finalJobResult
}

type finalJobResult struct {
	proverID string
	job      finalJob
	proof    *pb.FinalProof
	err      error
}

type proverCli struct {
	id      string
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

	proversCh chan proverCli
	proofsCh  chan jobResult
	finalCh   chan finalJob
	cutOff    <-chan time.Time
	srv       *grpc.Server
	ctx       context.Context
	exit      context.CancelFunc
}

// New creates a new aggregator.
func New(
	cfg Config,
	stateInterface stateInterface,
	ethTxManager ethTxManager,
	etherman etherman,
) (*AggregatorV2, error) {
	var profitabilityChecker aggregatorTxProfitabilityChecker
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
		proversCh:            make(chan proverCli),
		proofsCh:             make(chan jobResult),
		finalCh:              make(chan finalJob),
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

	address := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	a.srv = grpc.NewServer()
	pb.RegisterAggregatorServiceServer(a.srv, a)

	healthService := newHealthChecker()
	grpc_health_v1.RegisterHealthServer(a.srv, healthService)

	go a.orchestrate(ctx)

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

func (a *AggregatorV2) orchestrate(ctx context.Context) {
	finalProofCh := make(chan finalJobResult)
	timeOut := false

	for {
		select {
		case <-a.ctx.Done():
			return

		case <-a.cutOff:
			timeOut = true

		case finalResult := <-finalProofCh:
			inputProof := finalResult.job.proof
			finalProof := finalResult.proof

			log.Infof("Verfiying final proof with ethereum smart contract, batches %d-%d", inputProof.BatchNumber, inputProof.BatchNumberFinal)

			timeOut = false
			a.cutOff = time.After(a.cfg.IntervalToSendFinalProof.Duration)

			tx, err := a.Ethman.SendFinalProof(ctx, inputProof.BatchNumber-1, inputProof.BatchNumberFinal, finalProof, 0, nil, nil)
			if err != nil {
				log.Errorf("Error verifying final proof for batches %d-%d, err: %w", inputProof.BatchNumber, inputProof.BatchNumberFinal, err)
				continue
			}

			log.Infof("Final proof for batches%d-%d successfully verified in transction %v",
				inputProof.BatchNumber, inputProof.BatchNumberFinal, tx.Hash().String())

		case result := <-a.proofsCh:
			if timeOut && result.err == nil {
				log.Debug("time to send the final proof, checking if the current proof can be sent as final")

				err := a.validateFinalJob(ctx, &result)
				if errors.Is(err, ErrNotValidForFinal) {
					log.Debug(err.Error())

					// proof is not the final one, handle it normally
					err := a.handleProof(ctx, result)
					if err != nil {
						log.Error(err)
					}
					continue
				}
				if err != nil {
					log.Errorf("failed to validate job for final proof: %w", err)
					continue
				}

				fj := finalJob{
					proof:    result.proof,
					resultCh: finalProofCh,
				}

				select {
				case <-a.ctx.Done():
					return
				case a.finalCh <- fj:
					continue
				}
			}

			err := a.handleProof(ctx, result)
			if err != nil {
				log.Error(err)
				continue
			}

		case prover := <-a.proversCh:
			var pJob proverJob

			proof1, proof2, err := a.getAndLockProofsToAggregate(ctx)
			if err != nil {
				if errors.Is(err, state.ErrNotFound) {
					log.Debug("nothing to aggregate")
				} else {
					log.Errorf("failed to get proofs to aggregate: %v", err)
				}

				// try generate from batch
				batch, proof, err := a.getAndLockBatchToProve(ctx, prover.id)
				if errors.Is(err, state.ErrNotFound) {
					// nothing to generate, swallow the error
					log.Debug("nothing to generate")
					continue
				}
				if err != nil {
					log.Errorf("failed to get batch to prove: %v", err)
					continue
				}

				pJob = &generationJob{
					batch: batch,
					proof: proof,
				}
			} else {
				pJob = &aggregationJob{
					proof1: proof1,
					proof2: proof2,
				}
			}

			select {
			case <-a.ctx.Done():
				return
			case <-prover.ctx.Done():
				continue
			case prover.jobChan <- pJob:
			}
		}
	}
}

func (a *AggregatorV2) handleProof(ctx context.Context, result jobResult) error {
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		return fmt.Errorf("Failed to begin transaction to store proof aggregation result, err: %w", err)
	}

	switch job := result.job.(type) {
	case *aggregationJob:
		if result.err != nil {
			// failed job, rollback
			log.Errorf("Failed to aggregate proofs: %w", result.err)

			err := a.unlockProofsToAggregate(ctx, job.proof1, job.proof2, dbTx)
			if err != nil {
				dbTx.Rollback(ctx)
				return fmt.Errorf("Failed to unlock aggregated proofs: %w", err)
			}
			dbTx.Commit(ctx)
			return result.err
		}

		// Store proof
		err := a.State.AddGeneratedRecursiveProof(ctx, result.proof, dbTx)
		if err != nil {
			dbTx.Rollback(ctx)
			return fmt.Errorf("Failed to store proof aggregation result, err: %w", err)
		}

		// Delete aggregated proofs
		// TODO(pg): delete both with one call
		err = a.State.DeleteGeneratedRecursiveProof(ctx, job.proof1.BatchNumber, job.proof1.BatchNumberFinal, dbTx)
		if err != nil {
			dbTx.Rollback(ctx)
			return fmt.Errorf("Failed to delete aggregation input proof 1: %w", err)
		}
		err = a.State.DeleteGeneratedRecursiveProof(ctx, job.proof2.BatchNumber, job.proof2.BatchNumberFinal, dbTx)
		if err != nil {
			dbTx.Rollback(ctx)
			return fmt.Errorf("Failed to delete aggregation input proof 2: %w", err)
		}

	case *generationJob:
		if result.err != nil {
			// failed job, rollback
			log.Errorf("Failed to generate proof: %w", result.err)

			err := a.State.DeleteGeneratedRecursiveProof(ctx, job.proof.BatchNumber, job.proof.BatchNumberFinal, dbTx)
			if err != nil {
				dbTx.Rollback(ctx)
				return fmt.Errorf("Failed to delete proof in progress, err: %w", err)
			}
			dbTx.Commit(ctx)
			return result.err
		}

		// Store proof
		err := a.State.UpdateGeneratedRecursiveProof(ctx, result.proof, dbTx)
		if err != nil {
			dbTx.Rollback(ctx)
			return fmt.Errorf("Failed to to store batch proof result, err: %w", err)
		}
	}

	dbTx.Commit(ctx)
	return nil
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
	proverID := prover.ID()
	jobChan := make(chan proverJob)

	go func() {
		for {
			select {
			case <-a.ctx.Done():
				// server disconnected
				return
			case <-ctx.Done():
				// client disconnected
				return
			default:
				if !prover.IsIdle() {
					log.Debug("Prover ID %s is not idle", proverID)
					time.Sleep(a.cfg.IntervalFrequencyToGetProofGenerationState.Duration)
					continue
				}

				select {
				case <-a.ctx.Done():
					// server disconnected
					return
				case <-ctx.Done():
					// client disconnected
					return
				case finalJob := <-a.finalCh:
					log.Infof("Prover %s is going to be used to generate final proof for batches: %d-%d",
						proverID, finalJob.proof.BatchNumber, finalJob.proof.BatchNumberFinal)

					fjr := finalJobResult{
						proverID: proverID,
						job:      finalJob,
						proof:    nil,
						err:      nil,
					}

					finalProofID, err := prover.FinalProof(finalJob.proof.Proof)
					if err != nil {
						fjr.err = fmt.Errorf("Failed to instruct prover to prepare final proof: %w", err)
						log.Error(fjr.err)
						select {
						case <-a.ctx.Done():
							return
						case <-ctx.Done():
							return
						case finalJob.resultCh <- fjr:
							continue
						}
					}

					proof, err := prover.WaitFinalProof(ctx, finalProofID)
					if err != nil {
						fjr.err = fmt.Errorf("Failed to get final proof: %w", err)
						log.Error(fjr.err)
						select {
						case <-a.ctx.Done():
							return
						case <-ctx.Done():
							return
						case finalJob.resultCh <- fjr:
							continue
						}
					}

					fjr.proof = proof

					log.Infof("Final proof %s generated", finalProofID)

					select {
					case <-a.ctx.Done():
						return
					case <-ctx.Done():
						return
					default:
						go func() {
							finalJob.resultCh <- fjr
						}()
					}

				case proverJob := <-jobChan:
					switch job := proverJob.(type) {
					case *aggregationJob:
						log.Infof("Prover %s is going to be used to aggregate proofs: %d-%d and %d-%d",
							proverID, job.proof1.BatchNumber, job.proof1.BatchNumberFinal, job.proof2.BatchNumber, job.proof2.BatchNumberFinal)

						aggrProof := &state.RecursiveProof{
							BatchNumber:      job.proof1.BatchNumber,
							BatchNumberFinal: job.proof2.BatchNumberFinal,
							Prover:           &proverID,
							InputProver:      job.proof1.InputProver,
							Generating:       false,
						}

						jr := jobResult{
							proverID: proverID,
							job:      job,
							proof:    aggrProof,
							err:      nil,
						}

						aggrProofID, err := prover.AggregatedProof(job.proof1.Proof, job.proof2.Proof)
						if err != nil {
							jr.err = fmt.Errorf("Failed to instruct prover to generate aggregated proof: %w", err)
							log.Error(jr.err)
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}

						aggrProof.ProofID = &aggrProofID

						log.Infof("Proof ID for aggregated proof %d-%d: %v", aggrProof.BatchNumber, aggrProof.BatchNumberFinal, *aggrProof.ProofID)

						proof, err := prover.WaitRecursiveProof(ctx, aggrProofID)
						if err != nil {
							jr.err = fmt.Errorf("Failed to retrieve aggregated proof from prover: %w", err)
							log.Error(jr.err)
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}

						aggrProof.Proof = proof
						jr.proof.Generating = false

						select {
						case <-a.ctx.Done():
							return
						case <-ctx.Done():
							return
						case a.proofsCh <- jr:
						}

					case *generationJob:
						log.Infof("Prover %s is going to be used for batchNumber: %d", proverID, job.batch.BatchNumber)

						jr := jobResult{
							proverID: proverID,
							job:      job,
							proof:    job.proof,
							err:      nil,
						}
						jr.proof.Prover = &proverID

						log.Infof("Sending zki + batch to the prover, batchNumber: %d", job.batch.BatchNumber)
						inputProver, err := a.buildInputProver(ctx, job.batch)
						if err != nil {
							log.Errorf("Failed to build input prover, err: %v", err)
							jr.err = err
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}

						b, err := json.Marshal(inputProver)
						if err != nil {
							log.Errorf("Failed serialize input prover, err: %w", err)
							jr.err = err
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}
						jr.proof.InputProver = string(b)

						log.Infof("Sending a batch to the prover, OLDSTATEROOT: %s, OLDBATCHNUM: %d",
							inputProver.PublicInputs.OldStateRoot, inputProver.PublicInputs.OldBatchNum)

						genProofID, err := prover.BatchProof(inputProver)
						if err != nil {
							log.Errorf("Failed instruct prover to prove a batch: %w", err)
							jr.err = err
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}

						jr.proof.ProofID = &genProofID

						log.Infof("Proof ID for batchNumber %d: %v", job.proof.BatchNumber, *job.proof.ProofID)

						proof, err := prover.WaitRecursiveProof(ctx, *job.proof.ProofID)
						if err != nil {
							log.Errorf("Failed to get proof from prover, err: %v", err)
							jr.err = err
							select {
							case <-a.ctx.Done():
								return
							case <-ctx.Done():
								return
							case a.proofsCh <- jr:
								continue
							}
						}

						log.Infof("Batch proof %s generated", *job.proof.ProofID)

						jr.proof.Proof = proof
						jr.proof.Generating = false

						select {
						case <-a.ctx.Done():
							return
						case <-ctx.Done():
							return
						case a.proofsCh <- jr:
						}
					}

				default:
					a.proversCh <- proverCli{
						id:      proverID,
						ctx:     ctx,
						jobChan: jobChan,
					}
					time.Sleep(a.cfg.IntervalFrequencyToGetProofGenerationState.Duration)
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

func (a *AggregatorV2) validateFinalJob(ctx context.Context, inputJob *jobResult) error {
	log.Debug("Checking if network is synced")
	for !a.isSynced(ctx) {
		log.Debug("Waiting for synchronizer to sync...")
		continue
	}

	var lastBatchNumber uint64
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			return fmt.Errorf("Failed to get last verified batch: %w", err)
		}
		// no previous verified batches
		lastBatchNumber = 0
	} else {
		lastBatchNumber = lastVerifiedBatch.BatchNumber
	}

	batchNumberToVerify := lastBatchNumber + 1

	proof := inputJob.proof

	if proof.BatchNumber != batchNumberToVerify {
		return fmt.Errorf("%w: Proof batch number %d is not the following to last verfied batch number %d", ErrNotValidForFinal, proof.BatchNumber, batchNumberToVerify)
	}

	bComplete, err := a.State.CheckProofContainsCompleteSequences(ctx, proof, nil)
	if err != nil {
		return fmt.Errorf("Failed to check if proof %d-%d contains complete sequences", proof.BatchNumber, proof.BatchNumberFinal)
	}
	if !bComplete {
		return fmt.Errorf("%w: Recursive proof %d-%d does not contain complete sequences", ErrNotValidForFinal, proof.BatchNumber, proof.BatchNumberFinal)
	}

	return nil
}

func (a *AggregatorV2) unlockProofsToAggregate(ctx context.Context, proof1, proof2 *state.RecursiveProof, dbTx pgx.Tx) error {
	proof1.Generating = false
	err := a.State.UpdateGeneratedRecursiveProof(ctx, proof1, dbTx)
	if err != nil {
		return err
	}

	proof2.Generating = false
	err = a.State.UpdateGeneratedRecursiveProof(ctx, proof2, dbTx)
	if err != nil {
		return err
	}

	return nil
}

func (a *AggregatorV2) getAndLockProofsToAggregate(ctx context.Context) (*state.RecursiveProof, *state.RecursiveProof, error) {
	// Set proofs in aggregating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		return nil, nil, err
	}

	proof1, proof2, err := a.State.GetRecursiveProofsToAggregate(ctx, dbTx)
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

	dbTx.Commit(ctx)

	return proof1, proof2, nil
}

func (a *AggregatorV2) getAndLockBatchToProve(ctx context.Context, proverID string) (*state.Batch, *state.RecursiveProof, error) {
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

	proof := &state.RecursiveProof{
		BatchNumber:      batchToVerify.BatchNumber,
		BatchNumberFinal: batchToVerify.BatchNumber,
		Prover:           &proverID,
		Generating:       true,
	}

	// Avoid other provers to process the same batch
	err = a.State.AddGeneratedRecursiveProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to add batch proof, err: %v", err)
		return nil, nil, err
	}

	return batchToVerify, proof, nil
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
