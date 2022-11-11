package state

import pb2 "github.com/0xPolygonHermez/zkevm-node/aggregator_v2/pb"

// Proof struct
type Proof2 struct {
	BatchNumber      uint64
	BatchNumberFinal uint64
	Proof            string
	InputProver      *pb2.InputProver
	ProofID          *string
	Prover           *string
	Aggregating      bool
}
