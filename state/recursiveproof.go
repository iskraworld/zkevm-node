package state

// Proof struct
type RecursiveProof struct {
	BatchNumber      uint64
	BatchNumberFinal uint64
	Proof            string
	InputProver      string
	ProofID          *string
	Prover           *string
	Generating       bool
}
