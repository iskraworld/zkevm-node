package prover

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/aggregator/pb"
	"github.com/0xPolygonHermez/zkevm-node/config/types"
	"github.com/0xPolygonHermez/zkevm-node/log"
)

var ErrBadProverResponse = errors.New("Prover returned wrong type for response")

// Prover abstraction of the grpc prover client.
type Prover struct {
	id                                         string
	IntervalFrequencyToGetProofGenerationState types.Duration
	stream                                     pb.AggregatorService_ChannelServer
}

// New returns a new Prover instance.
func New(stream pb.AggregatorService_ChannelServer, intervalFrequencyToGetProofGenerationState types.Duration) (*Prover, error) {
	p := &Prover{
		stream: stream,
		IntervalFrequencyToGetProofGenerationState: intervalFrequencyToGetProofGenerationState,
	}
	status, err := p.Status()
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve prover id %w", err)
	}
	p.id = status.ProverId
	return p, nil
}

// ID returns the Prover ID.
func (p *Prover) ID() string { return p.id }

// Status gets the prover status.
func (p *Prover) Status() (*pb.GetStatusResponse, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GetStatusRequest{
			GetStatusRequest: &pb.GetStatusRequest{},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return nil, err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GetStatusResponse); ok {
		return msg.GetStatusResponse, nil
	}
	return nil, fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_GetStatusResponse{}, res.Response)
}

// IsIdle returns true if the prover is idling.
func (p *Prover) IsIdle() bool {
	status, err := p.Status()
	if err != nil {
		log.Warnf("Error asking status for prover ID %s: %w", p.ID(), err)
		return false
	}
	return status.Status == pb.GetStatusResponse_IDLE
}

// BatchProof instructs the prover to generate a batch proof for the provided
// input. It returns the ID of the proof being computed.
func (p *Prover) BatchProof(input *pb.InputProver) (string, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenBatchProofRequest{
			GenBatchProofRequest: &pb.GenBatchProofRequest{Input: input},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return "", err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenBatchProofResponse); ok {
		switch msg.GenBatchProofResponse.Result {
		case pb.Result_UNSPECIFIED:
			return "", fmt.Errorf("Failed to generate proof %s, input %v", msg.GenBatchProofResponse.String(), input)
		case pb.Result_OK:
			return msg.GenBatchProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", fmt.Errorf("Failed to generate proof %s, input %v", msg.GenBatchProofResponse.String(), input)
		case pb.Result_INTERNAL_ERROR:
			return "", fmt.Errorf("Failed to generate proof %s, input %v", msg.GenBatchProofResponse.String(), input)
		default:
			return "", fmt.Errorf("Failed to generate proof %s, input %v", msg.GenBatchProofResponse.String(), input)
		}
	}

	return "", fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_GenBatchProofResponse{}, res.Response)
}

// AggregatedProof instructs the prover to generate an aggregated proof from
// the two inputs provided. It returns the ID of the proof being computed.
func (p *Prover) AggregatedProof(inputProof1, inputProof2 string) (string, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenAggregatedProofRequest{
			GenAggregatedProofRequest: &pb.GenAggregatedProofRequest{
				RecursiveProof_1: inputProof1,
				RecursiveProof_2: inputProof2,
			},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return "", err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenAggregatedProofResponse); ok {
		switch msg.GenAggregatedProofResponse.Result {
		case pb.Result_UNSPECIFIED:
			return "", fmt.Errorf("Failed to aggregate proofs %s, input 1 %s, input 2 %s", msg.GenAggregatedProofResponse.String(), inputProof1, inputProof2)
		case pb.Result_OK:
			return msg.GenAggregatedProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", fmt.Errorf("Failed to aggregate proofs %s, input 1 %s, input 2 %s", msg.GenAggregatedProofResponse.String(), inputProof1, inputProof2)
		case pb.Result_INTERNAL_ERROR:
			return "", fmt.Errorf("Failed to aggregate proofs %s, input 1 %s, input 2 %s", msg.GenAggregatedProofResponse.String(), inputProof1, inputProof2)
		default:
			return "", fmt.Errorf("Failed to aggregate proofs %s, input 1 %s, input 2 %s", msg.GenAggregatedProofResponse.String(), inputProof1, inputProof2)
		}
	}
	return "", fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_GenAggregatedProofResponse{}, res.Response)
}

// FinalProof instructs the prover to generate a final proof for the given
// input. It returns the ID of the proof being computed.
func (p *Prover) FinalProof(inputProof string) (string, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenFinalProofRequest{
			GenFinalProofRequest: &pb.GenFinalProofRequest{RecursiveProof: inputProof},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return "", err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenFinalProofResponse); ok {
		switch msg.GenFinalProofResponse.Result {
		case pb.Result_UNSPECIFIED:
			return "", fmt.Errorf("Failed to generate final proof %s, input %s", msg.GenFinalProofResponse.String(), inputProof)
		case pb.Result_OK:
			return msg.GenFinalProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", fmt.Errorf("Failed to generate final proof %s, input %s", msg.GenFinalProofResponse.String(), inputProof)
		case pb.Result_INTERNAL_ERROR:
			return "", fmt.Errorf("Failed to generate final proof %s, input %s", msg.GenFinalProofResponse.String(), inputProof)
		default:
			return "", fmt.Errorf("Failed to generate final proof %s, input %s", msg.GenFinalProofResponse.String(), inputProof)
		}
	}
	return "", fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_GenFinalProofResponse{}, res.Response)
}

// CancelProofRequest asks the prover to stop the generation of the proof
// matching the provided proofID.
func (p *Prover) CancelProofRequest(proofID string) error {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_CancelRequest{
			CancelRequest: &pb.CancelRequest{Id: proofID},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_CancelResponse); ok {
		// TODO(pg): handle all cases
		switch msg.CancelResponse.Result {
		case pb.Result_UNSPECIFIED:
			return fmt.Errorf("Failed to cancel proof id [%s] %s", proofID, msg.CancelResponse.String())
		case pb.Result_OK:
			return nil
		case pb.Result_ERROR:
			return fmt.Errorf("Failed to cancel proof id [%s] %s", proofID, msg.CancelResponse.String())
		case pb.Result_INTERNAL_ERROR:
			return fmt.Errorf("Failed to cancel proof id [%s] %s", proofID, msg.CancelResponse.String())
		default:
			return fmt.Errorf("Failed to cancel proof id [%s] %s", proofID, msg.CancelResponse.String())
		}
	}
	return fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_CancelResponse{}, res.Response)
}

// WaitRecursiveProof waits for a recursive proof to be generated by the prover
// and returns it.
func (p *Prover) WaitRecursiveProof(ctx context.Context, proofID string) (string, error) {
	res, err := p.waitProof(ctx, proofID)
	if err != nil {
		return "", err
	}
	resProof := res.Proof.(*pb.GetProofResponse_RecursiveProof)
	return resProof.RecursiveProof, nil
}

// WaitFinalProof waits for the final proof to be generated by the prover and
// returns it.
func (p *Prover) WaitFinalProof(ctx context.Context, proofID string) (*pb.FinalProof, error) {
	res, err := p.waitProof(ctx, proofID)
	if err != nil {
		return nil, err
	}
	resProof := res.Proof.(*pb.GetProofResponse_FinalProof)
	return resProof.FinalProof, nil
}

// waitProof waits for a proof to be generated by the prover and returns the
// prover response.
func (p *Prover) waitProof(ctx context.Context, proofID string) (*pb.GetProofResponse, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GetProofRequest{
			GetProofRequest: &pb.GetProofRequest{
				// TODO(pg): set Timeout field?
				Id: proofID,
			},
		},
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			res, err := p.call(req)
			if err != nil {
				return nil, err
			}
			if msg, ok := res.Response.(*pb.ProverMessage_GetProofResponse); ok {
				switch msg.GetProofResponse.Result {
				case pb.GetProofResponse_PENDING:
					time.Sleep(p.IntervalFrequencyToGetProofGenerationState.Duration)
					continue
				case pb.GetProofResponse_UNSPECIFIED:
					return nil, fmt.Errorf("Failed to get proof ID: %s, prover response: %s", proofID, msg.GetProofResponse.String())
				case pb.GetProofResponse_COMPLETED_OK:
					return msg.GetProofResponse, nil
				case pb.GetProofResponse_ERROR, pb.GetProofResponse_COMPLETED_ERROR:
					return nil, fmt.Errorf("Failed to get proof with ID %s, prover response: %s", proofID, msg.GetProofResponse.String())
				case pb.GetProofResponse_INTERNAL_ERROR:
					return nil, fmt.Errorf("Failed to get proof ID: %s, prover response: %s", proofID, msg.GetProofResponse.String())
				case pb.GetProofResponse_CANCEL:
					return nil, fmt.Errorf("Proof generation was cancelled for proof ID %s, prover response: %s", proofID, msg.GetProofResponse.String())
				default:
					return nil, fmt.Errorf("Failed to get proof ID: %s, prover response: %s", proofID, msg.GetProofResponse.String())
				}
			}
			return nil, fmt.Errorf("%w, wanted %T, got %T", ErrBadProverResponse, &pb.ProverMessage_GetProofResponse{}, res.Response)
		}
	}
}

// call sends a message to the prover and waits to receive the response over
// the connection stream.
func (p *Prover) call(req *pb.AggregatorMessage) (*pb.ProverMessage, error) {
	if err := p.stream.Send(req); err != nil {
		return nil, err
	}
	res, err := p.stream.Recv()
	if err != nil {
		return nil, err
	}
	return res, nil
}
