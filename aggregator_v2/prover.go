package aggregatorv2

import (
	"context"
	"errors"

	"github.com/0xPolygonHermez/zkevm-node/aggregator_v2/pb"
)

// Prover abstraction of the grpc prover client.
type Prover struct {
	ID     string
	stream pb.AggregatorService_ChannelServer
}

// NewProver returns a new Prover instance.
func NewProver(stream pb.AggregatorService_ChannelServer) (*Prover, error) {
	p := &Prover{stream: stream}
	status, err := p.Status()
	if err != nil {
		return nil, err
	}
	p.ID = status.ProverId
	return p, nil
}

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

// BatchProof instructs the prover to generate a batch proof. It returns the ID
// of the proof being computed.
func (p *Prover) BatchProof(input *pb.InputProver) (*pb.GenBatchProofResponse, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenBatchProofRequest{
			GenBatchProofRequest: &pb.GenBatchProofRequest{Input: input},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return nil, err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenBatchProofResponse); ok {
		// TODO(pg): handle all cases
		switch msg.GenBatchProofResponse.Result {
		case pb.Result_UNSPECIFIED:
		case pb.Result_OK:
			return msg.GenBatchProofResponse, nil
		case pb.Result_ERROR:
		case pb.Result_INTERNAL_ERROR:
		}
	}
	return nil, errors.New("bad response") // FIXME(pg)
}

// AggregatedProof instructs the prover to generate an aggregated proof. It
// returns the ID of the proof being computed.
func (p *Prover) AggregatedProof(in1, in2 string) (*pb.GenAggregatedProofResponse, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenAggregatedProofRequest{
			GenAggregatedProofRequest: &pb.GenAggregatedProofRequest{Input_1: in1, Input_2: in2},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return nil, err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenAggregatedProofResponse); ok {
		// TODO(pg): handle all cases
		switch msg.GenAggregatedProofResponse.Result {
		case pb.Result_UNSPECIFIED:
		case pb.Result_OK:
			return msg.GenAggregatedProofResponse, nil
		case pb.Result_ERROR:
		case pb.Result_INTERNAL_ERROR:
		}
	}
	return nil, errors.New("bad response") // FIXME(pg)
}

// FinalProof instructs the prover to generate a final proof. It returns the ID
// of the proof being computed.
func (p *Prover) FinalProof(in string) (*pb.GenFinalProofResponse, error) {
	req := &pb.AggregatorMessage{
		Request: &pb.AggregatorMessage_GenFinalProofRequest{
			GenFinalProofRequest: &pb.GenFinalProofRequest{Input: in},
		},
	}
	res, err := p.call(req)
	if err != nil {
		return nil, err
	}
	if msg, ok := res.Response.(*pb.ProverMessage_GenFinalProofResponse); ok {
		// TODO(pg): handle all cases
		switch msg.GenFinalProofResponse.Result {
		case pb.Result_UNSPECIFIED:
		case pb.Result_OK:
			return msg.GenFinalProofResponse, nil
		case pb.Result_ERROR:
		case pb.Result_INTERNAL_ERROR:
		}
	}
	return nil, errors.New("bad response") // FIXME(pg)
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
		case pb.Result_INTERNAL_ERROR:
		}
	}
	return errors.New("bad response") // FIXME(pg)
}

// WaitProof waits for a proof to be generated by the prover and returns it.
func (p *Prover) WaitProof(ctx context.Context, proofID string) (*pb.GetProofResponse, error) {
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
				// TODO(pg): handle all cases
				switch msg.GetProofResponse.Result {
				case pb.GetProofResponse_UNSPECIFIED:
				case pb.GetProofResponse_COMPLETED_OK:
					return msg.GetProofResponse, nil
				case pb.GetProofResponse_ERROR:
				case pb.GetProofResponse_COMPLETED_ERROR:
				case pb.GetProofResponse_PENDING:
					continue
				case pb.GetProofResponse_INTERNAL_ERROR:
				case pb.GetProofResponse_CANCEL:
				}
			}
			return nil, errors.New("bad response") // FIXME(pg)
		}
	}
}

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
