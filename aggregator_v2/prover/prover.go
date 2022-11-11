package prover

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/aggregator_v2/pb"
	"github.com/0xPolygonHermez/zkevm-node/log"
)

// Prover abstraction of the grpc prover client.
type Prover struct {
	id     string
	cfg    *Config
	stream pb.AggregatorService_ChannelServer
}

// New returns a new Prover instance.
func New(cfg *Config, stream pb.AggregatorService_ChannelServer) (*Prover, error) {
	p := &Prover{
		cfg:    cfg,
		stream: stream,
	}
	status, err := p.Status()
	if err != nil {
		return nil, err
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
	return nil, errors.New("bad response") // FIXME(pg)
}

// IsIdle returns true if the prover is idling.
func (p *Prover) IsIdle() bool {
	status, err := p.Status()
	if err != nil {
		log.Warnf("error asking status for prover ID %s: %w", p.ID, err)
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
			// TODO(pg): handle this case
		case pb.Result_OK:
			return msg.GenBatchProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", errors.New("Prover error")
		case pb.Result_INTERNAL_ERROR:
			return "", errors.New("Prover internal error")
		}
	}
	return "", errors.New("bad response") // FIXME(pg)
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
			// TODO(pg): handle this case
		case pb.Result_OK:
			return msg.GenAggregatedProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", errors.New("Prover error")
		case pb.Result_INTERNAL_ERROR:
			return "", errors.New("Prover internal error")
		}
	}
	return "", errors.New("bad response") // FIXME(pg)
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
			// TODO(pg): handle this case
		case pb.Result_OK:
			return msg.GenFinalProofResponse.Id, nil
		case pb.Result_ERROR:
			return "", errors.New("Prover error")
		case pb.Result_INTERNAL_ERROR:
			return "", errors.New("Prover internal error")
		}
	}
	return "", errors.New("bad response") // FIXME(pg)
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
		case pb.Result_OK:
			return nil
		case pb.Result_ERROR:
			return errors.New("Prover error")
		case pb.Result_INTERNAL_ERROR:
			return errors.New("Prover internal error")
		}
	}
	return errors.New("bad response") // FIXME(pg)
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
				case pb.GetProofResponse_UNSPECIFIED:
					// TODO(pg): handle this case?
				case pb.GetProofResponse_COMPLETED_OK:
					return msg.GetProofResponse, nil
				case pb.GetProofResponse_ERROR, pb.GetProofResponse_COMPLETED_ERROR:
					log.Fatalf("failed to get proof with ID %s", proofID)
				case pb.GetProofResponse_PENDING:
					time.Sleep(p.cfg.IntervalFrequencyToGetProofGenerationState.Duration)
					continue
				case pb.GetProofResponse_INTERNAL_ERROR:
					return nil, fmt.Errorf("failed to generate proof ID: %s, ResGetProofState: %v", proofID, msg.GetProofResponse)
				case pb.GetProofResponse_CANCEL:
					log.Warnf("proof generation was cancelled for proof ID %s", proofID)
					return msg.GetProofResponse, nil
				}
			}
			return nil, errors.New("bad response") // FIXME(pg)
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
