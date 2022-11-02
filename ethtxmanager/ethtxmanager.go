// Package ethtxmanager handles ethereum transactions:  It makes
// calls to send and to aggregate batch, checks possible errors, like wrong nonce or gas limit too low
// and make correct adjustments to request according to it. Also, it tracks transaction receipt and status
// of tx in case tx is rejected and send signals to sequencer/aggregator to resend sequence/batch
package ethtxmanager

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
)

// ErrTimestampOutsideRange represents an error when a tx to send a sequence
// to the roll-up contains a sequence that doesn't match the expected timestamp
// stored in the roll-up
const ErrTimestampOutsideRange = "Timestamp must be inside range"

// Client for eth tx manager
type Client struct {
	cfg    Config
	state  stateInterface
	ethMan etherman
}

// New creates new eth tx manager
func New(cfg Config, st stateInterface, ethMan etherman) *Client {
	return &Client{
		cfg:    cfg,
		state:  st,
		ethMan: ethMan,
	}
}

// SyncPendingSequences loads pending sequences from the state and
// sync them with PoE on L1
func (c *Client) SyncPendingSequences() {
	c.groupSequences()
	c.syncSequences()
}

// SyncPendingProofs loads pending proofs from the state and
// sync them with PoE on L1
func (c *Client) SyncPendingProofs() {
	ctx := context.Background()
	// get all pending proofs
	pendingProofs, err := c.state.GetPendingProofs(ctx, nil)
	if err != nil {
		log.Errorf("failed to get pending proofs: %v", err)
		return
	}
	// generate a l1 transaction for all pending proofs
	for _, pendingProof := range pendingProofs {
		if pendingProof.TxHash == nil {
			tx, err := c.ethMan.VerifyBatch(ctx, pendingProof.BatchNumber, pendingProof.Proof, 0, nil, nil)
			if err != nil {
				log.Errorf("failed to send tx to verify batch for batch number %v: %v", pendingProof.BatchNumber, err)
				continue
			}

			err = c.state.UpdateProofTx(ctx, pendingProof.BatchNumber, tx.Hash(), nil)
			if err != nil {
				log.Errorf("failed to update tx to verify batch for batch number %v, new tx hash %v, nonce %v, err: %v", pendingProof.BatchNumber, tx.Hash().String(), tx.Nonce(), err)
				continue
			}

			continue
		}

		if confirmed := c.checkProofConfirmation(ctx, pendingProof); !confirmed {
			c.tryReviewProofTx(ctx, pendingProof)
		}
	}
}

// groupSequences build sequence groups with sequences without group
func (c *Client) groupSequences() {
	ctx := context.Background()

	// get sequences without group
	sequencesWithoutGroup, err := c.state.GetSequencesWithoutGroup(ctx, nil)
	if err != nil {
		log.Errorf("failed to get sequences without group: %v", err)
		return
	}

	// if there is no sequence without group, returns
	if len(sequencesWithoutGroup) == 0 {
		return
	}

	// send the sequences to create the tx
	var tx *types.Transaction
	confirmed := false
	for {
		tx, err = c.ethMan.SequenceBatches(ctx, sequencesWithoutGroup, 0, nil, nil)
		if err != nil {
			// is the amount of sequences causes oversized, reduce the sequences by one
			if err.Error() == core.ErrOversizedData.Error() {
				sequencesWithoutGroup = sequencesWithoutGroup[:len(sequencesWithoutGroup)-1]
				// if the error is the timestamp outside the range and there are multiple sequences, try to send only one
			} else if strings.Contains(err.Error(), ErrTimestampOutsideRange) && len(sequencesWithoutGroup) > 1 {
				sequencesWithoutGroup = sequencesWithoutGroup[0:1]
				// if the error is the timestamp outside the range and there is a single sequence, mark it as confirmed because it was already sequenced
			} else if strings.Contains(err.Error(), ErrTimestampOutsideRange) && len(sequencesWithoutGroup) == 1 {
				confirmed = true
				break
			} else {
				log.Errorf("failed to send sequence batches: %v", err)
				return
			}
		} else {
			break
		}
	}

	// create a pending sequence group with sequences and tx
	sequenceGroup := state.SequenceGroup{
		TxHash:       tx.Hash(),
		TxNonce:      tx.Nonce(),
		Status:       state.SequenceGroupStatusPending,
		CreatedAt:    time.Now(),
		BatchNumbers: make([]uint64, 0, len(sequencesWithoutGroup)),
	}
	for _, sequence := range sequencesWithoutGroup {
		sequenceGroup.BatchNumbers = append(sequenceGroup.BatchNumbers, sequence.BatchNumber)
	}

	// persist sequence group to start monitoring this tx
	err = c.state.AddSequenceGroup(ctx, sequenceGroup, nil)
	if err != nil {
		log.Errorf("failed to create sequence group: %v", err)
		return
	}
	log.Infof("sequence group created for batches %v: %v", sequenceGroup.BatchNumbers, sequenceGroup.TxHash.String())

	if confirmed {
		err := c.state.SetSequenceGroupAsConfirmed(ctx, sequenceGroup.TxHash, nil)
		if err != nil {
			log.Errorf("failed to set sequence group as confirmed for tx %v: %v", sequenceGroup.TxHash.String(), err)
		}
		return
	}
}

func (c *Client) syncSequences() {
	ctx := context.Background()

	pendingSequenceGroups, err := c.state.GetPendingSequenceGroups(ctx, nil)
	if err != nil {
		log.Errorf("failed to get pending sequence groups: %v", err)
		return
	}

	for _, pendingSequenceGroup := range pendingSequenceGroups {
		if confirmed := c.checkSequenceGroupConfirmation(ctx, pendingSequenceGroup); !confirmed {
			c.tryReviewSequenceGroupTx(ctx, pendingSequenceGroup)
		}
	}
}

func (c *Client) checkSequenceGroupConfirmation(ctx context.Context, sequenceGroup state.SequenceGroup) bool {
	log.Infof("trying to confirm sequence for batches %v: %v", sequenceGroup.BatchNumbers, sequenceGroup.TxHash.String())
	receipt, err := c.ethMan.GetTxReceipt(ctx, sequenceGroup.TxHash)
	if err != nil && !errors.Is(err, ethereum.NotFound) {
		log.Errorf("failed to get sequence group for batches %v tx receipt, hash %v: %v", sequenceGroup.BatchNumbers, sequenceGroup.TxHash.String(), err)
		return false
	}
	if receipt != nil && receipt.Status == types.ReceiptStatusSuccessful {
		err := c.state.SetSequenceGroupAsConfirmed(ctx, sequenceGroup.TxHash, nil)
		if err != nil {
			log.Errorf("failed to set sequence group as confirmed for batches %v tx %v: %v", sequenceGroup.BatchNumbers, sequenceGroup.TxHash.String(), err)
			return false
		}
		log.Infof("sequence group for batches %v confirmed", sequenceGroup.BatchNumbers)
		return true
	}
	log.Infof("sequence group for batches %v not confirmed yet", sequenceGroup.BatchNumbers)
	return false
}

func (c *Client) tryReviewSequenceGroupTx(ctx context.Context, sequenceGroup state.SequenceGroup) {
	// if it was not mined yet, check if the timeout since the last time the group was update has expired
	lastTimeSequenceWasUpdated := sequenceGroup.CreatedAt
	if sequenceGroup.UpdatedAt != nil {
		lastTimeSequenceWasUpdated = *sequenceGroup.UpdatedAt
	}
	// if the time to review the tx has expired, we review it
	if time.Since(lastTimeSequenceWasUpdated) >= c.cfg.IntervalToReviewSendBatchTx.Duration {
		log.Infof("reviewing sequence group tx for batches %v due to long time waiting for it to be confirmed", sequenceGroup.BatchNumbers)

		sequences, err := c.state.GetSequencesByBatchNums(ctx, sequenceGroup.BatchNumbers, nil)
		if err != nil {
			log.Errorf("failed to get sequences by batch numbers: %v", err)
			return
		}

		nonce := big.NewInt(0).SetUint64(sequenceGroup.TxNonce)
		// using the same nonce, create a new transaction, this will make the gas to be
		// recalculated with the current prices of the network
		tx, err := c.ethMan.SequenceBatches(ctx, sequences, 0, nil, nonce)
		if err != nil {
			// if the tx is already know, refresh the update date to give it more time to get mined
			if errors.Is(err, core.ErrAlreadyKnown) {
				err := c.state.UpdateSequenceGroupTx(ctx, sequenceGroup.TxHash, sequenceGroup.TxHash, nil)
				if err != nil {
					log.Errorf("give it more time to the sequence group related to the batches %v to get mined: %v", sequenceGroup.BatchNumbers, err)
				}
				return
			}
			log.Errorf("failed to resend sequence tx for batches %v: %v", sequenceGroup.BatchNumbers, err)
			return
		}

		log.Infof("updating tx for sequence group related to batches %v from %v to %v",
			sequenceGroup.BatchNumbers, sequenceGroup.TxHash.String(), tx.Hash().String())

		err = c.state.UpdateSequenceGroupTx(ctx, sequenceGroup.TxHash, tx.Hash(), nil)
		if err != nil {
			log.Errorf("failed to update sequence group from %v to %v: %v", sequenceGroup.TxHash.String(), tx.Hash().String(), err)
			return
		}
	}
}

func (c *Client) checkProofConfirmation(ctx context.Context, proof state.Proof) bool {
	log.Infof("trying to confirm proof for batch %v: %v", proof.BatchNumber, proof.TxHash.String())
	receipt, err := c.ethMan.GetTxReceipt(ctx, *proof.TxHash)
	if err != nil && !errors.Is(err, ethereum.NotFound) {
		log.Errorf("failed to get tx receipt for proof for batch %v, hash %v: %v", proof.BatchNumber, proof.TxHash.String(), err)
		return false
	}
	if receipt != nil && receipt.Status == types.ReceiptStatusSuccessful {
		err := c.state.SetProofAsConfirmed(ctx, proof.BatchNumber, nil)
		if err != nil {
			log.Errorf("failed to set proof as confirmed for batch %v tx %v: %v", proof.BatchNumber, proof.TxHash.String(), err)
			return false
		}
		log.Infof("proof for batch %v confirmed", proof.BatchNumber)
		return true
	}
	log.Infof("proof for batch %v not confirmed yet", proof.BatchNumber)
	return false
}

func (c *Client) tryReviewProofTx(ctx context.Context, proof state.Proof) {
	// if it was not mined yet, check if the timeout since the last time the proof was update has expired
	lastTimeSequenceWasUpdated := proof.CreatedAt
	if proof.UpdatedAt != nil {
		lastTimeSequenceWasUpdated = *proof.UpdatedAt
	}
	// if the time to review the tx has expired, we review it
	if time.Since(lastTimeSequenceWasUpdated) >= c.cfg.IntervalToReviewVerifyBatchTx.Duration {
		log.Infof("reviewing proof tx for batch %v due to long time waiting for it to be confirmed", proof.BatchNumber)

		nonce := big.NewInt(0).SetUint64(*proof.TxNonce)
		// using the same nonce, create a new transaction, this will make the gas to be
		// recalculated with the current prices of the network
		tx, err := c.ethMan.VerifyBatch(ctx, proof.BatchNumber, proof.Proof, 0, nil, nonce)
		if err != nil {
			// if the tx is already know, refresh the update date to give it more time to get mined
			if errors.Is(err, core.ErrAlreadyKnown) {
				err := c.state.UpdateProofTx(ctx, proof.BatchNumber, *proof.TxHash, nil)
				if err != nil {
					log.Errorf("give it more time to the proof related to the batch %v to get mined: %v", proof.BatchNumber, err)
				}
				return
			}
			log.Errorf("failed to resend tx to verify batch %v: %v", proof.BatchNumber, err)
			return
		}

		log.Infof("updating tx for proof related to batch %v from %v to %v",
			proof.BatchNumber, proof.TxHash.String(), tx.Hash().String())

		err = c.state.UpdateSequenceGroupTx(ctx, *proof.TxHash, tx.Hash(), nil)
		if err != nil {
			log.Errorf("failed to update proof tx from %v to %v: %v", proof.TxHash.String(), tx.Hash().String(), err)
			return
		}
	}
}
