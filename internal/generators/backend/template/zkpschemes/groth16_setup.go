package zkpschemes

// Groth16Setup ...
const Groth16Setup = `

import (
	{{ template "import_curve" . }}
	{{ template "import_backend" . }}
	{{ template "import_fft" . }}
	"github.com/consensys/gnark/internal/utils/parallel"
)

// ProvingKey is used by a Groth16 prover to encode a proof of a statement
type ProvingKey struct {
	// [α]1, [β]1, [δ]1
	// [A(t)]1, [B(t)]1, [Kpk(t)]1, [Z(t)]1
	G1 struct {
		Alpha, Beta, Delta curve.G1Affine
		A, B, Z            []curve.G1Affine
		K                  []curve.G1Affine // the indexes correspond to the private wires
	}

	// [β]2, [δ]2, [B(t)]2
	G2 struct {
		Beta, Delta curve.G2Affine
		B           []curve.G2Affine
	}
	
	Domain fft.Domain
}

// VerifyingKey is used by a Groth16 verifier to verify the validity of a proof and a statement
type VerifyingKey struct {
	// e(α, β)
	E curve.GT

	// -[γ]2, -[δ]2
	// note: storing GammaNeg and DeltaNeg instead of Gamma and Delta
	// see proof.Verify() for more details
	G2 struct {
		GammaNeg, DeltaNeg curve.G2Affine
	}

	// [Kvk]1
	G1 struct {
		K []curve.G1Affine // The indexes correspond to the public wires
	}

	PublicInputs []string // maps the name of the public input
}

// Setup constructs the SRS
func Setup(r1cs *{{toLower .Curve}}backend.R1CS, pk *ProvingKey, vk *VerifyingKey) {

	/*
		Setup
		-----
		To build the verifying keys:
		- compile the r1cs system -> the number of gates is len(GateOrdering)+len(PureStructuralConstraints)+len(InpureStructuralConstraints)
		- loop through the ordered computational constraints (=gate in r1cs system structure), eValuate A(X), B(X), C(X) with simple formula (the gate number is the current iterator)
		- loop through the inpure structural constraints, eValuate A(X), B(X), C(X) with simple formula, the gate number is len(gateOrdering)+ current iterator
		- loop through the pure structural constraints, eValuate A(X), B(X), C(X) with simple formula, the gate number is len(gateOrdering)+len(InpureStructuralConstraints)+current iterator
	*/

	// get R1CS nb constraints, wires and public/private inputs
	nbWires := r1cs.NbWires
	nbPublicWires := r1cs.NbPublicWires
	nbPrivateWires := r1cs.NbWires - r1cs.NbPublicWires
	nbConstraints := r1cs.NbConstraints

	// Setting group for fft
	domain := fft.NewDomain( nbConstraints)

	// Set public inputs in Verifying Key (Verify does not need the R1CS data structure)
	vk.PublicInputs = r1cs.PublicWires

	// samples toxic waste
	toxicWaste := sampleToxicWaste()

	// Setup coeffs to compute pk.G1.A, pk.G1.B, pk.G1.K
	A, B, C := setupABC(r1cs, domain, toxicWaste)

	// To fill in the Proving and Verifying keys, we need to perform a lot of ecc scalar multiplication (with generator)
	// and convert the resulting points to affine
	// this is done using the curve.BatchScalarMultiplicationGX API, which takes as input the base point
	// (in our case the generator) and the list of scalars, and outputs a list of points (len(points) == len(scalars))
	// to use this batch call, we need to order our scalars in the same slice
	// we have 1 batch call for G1 and 1 batch call for G1
	// scalars are fr.Element in non montgomery form
	_, _, g1, g2 := curve.Generators()

	// ---------------------------------------------------------------------------------------------
	// G1 scalars

	// the G1 scalars are ordered (arbitrary) as follow:
	// 
	// [[α], [β], [δ], [A(i)], [B(i)], [pk.K(i)], [Z(i)], [vk.K(i)]]
	// len(A) == len(B) == nbWires
	// len(pk.K) == nbPrivateWires
	// len(vk.K) == nbPublicWires
	// len(Z) == domain.Cardinality

	// compute scalars for pkK and vkK
	pkK := make([]fr.Element,nbPrivateWires)
	vkK := make([]fr.Element,nbPublicWires)


	var t0, t1 fr.Element
	for i := 0; i < nbPrivateWires; i++ {
		t1.Mul(&A[i], &toxicWaste.beta)
		t0.Mul(&B[i], &toxicWaste.alpha)
		t1.Add(&t1, &t0).
			Add(&t1, &C[i]).
			Div(&t1, &toxicWaste.delta)
		pkK[i] = t1.ToRegular()
	}

	for i := 0; i < nbPublicWires; i++ {
		t1.Mul(&A[i+nbPrivateWires], &toxicWaste.beta)
		t0.Mul(&B[i+nbPrivateWires], &toxicWaste.alpha)
		t1.Add(&t1, &t0).
			Add(&t1, &C[i+nbPrivateWires]).
			Div(&t1, &toxicWaste.gamma)
		vkK[i] = t1.ToRegular()
	}

	// convert A and B to regular form
	for i:=0; i < nbWires; i++ {
		A[i].FromMont()
	}
	for i:=0; i < nbWires; i++ {
		B[i].FromMont()
	}

	// Z part of the proving key (scalars)
	Z := make([]fr.Element, domain.Cardinality)
	one := fr.One()
	var zdt fr.Element

	zdt.Exp(toxicWaste.t, new(big.Int).SetUint64(uint64(domain.Cardinality))).
		Sub(&zdt, &one).
		Div(&zdt, &toxicWaste.delta) // sets Zdt to Zdt/delta

	
	for i := 0; i < domain.Cardinality; i++ {
		Z[i] = zdt.ToRegular()
		zdt.MulAssign(&toxicWaste.t)
	}


	// compute our batch scalar multiplication with g1 elements
	g1Scalars := make([]fr.Element,0, (nbWires*3) + domain.Cardinality + 3)
	g1Scalars = append(g1Scalars, toxicWaste.alphaReg, toxicWaste.betaReg, toxicWaste.deltaReg)
	g1Scalars = append(g1Scalars, A...)
	g1Scalars = append(g1Scalars, B...)
	g1Scalars = append(g1Scalars, pkK...)
	g1Scalars = append(g1Scalars, Z...)
	g1Scalars = append(g1Scalars, vkK...)

	g1PointsAff := curve.BatchScalarMultiplicationG1(&g1, g1Scalars)

	// sets pk: [α]1, [β]1, [δ]1
	pk.G1.Alpha = g1PointsAff[0]
	pk.G1.Beta = g1PointsAff[1]
	pk.G1.Delta = g1PointsAff[2]


	offset := 3
	pk.G1.A = g1PointsAff[offset:offset+nbWires]
	offset += nbWires

	pk.G1.B = g1PointsAff[offset:offset+nbWires]
	offset += nbWires

	pk.G1.K = g1PointsAff[offset:offset+nbPrivateWires]
	offset += nbPrivateWires

	pk.G1.Z = g1PointsAff[offset:offset+domain.Cardinality]
	bitReverse(pk.G1.Z)
	
	offset += domain.Cardinality

	vk.G1.K = g1PointsAff[offset:]

	// ---------------------------------------------------------------------------------------------
	// G2 scalars

	// the G2 scalars are ordered as follow:
	//
	// [[B(i)], [β], [δ], [γ]]
	// len(B) == nbWires


	// compute our batch scalar multiplication with g2 elements
	g2Scalars := append(B, toxicWaste.betaReg, toxicWaste.deltaReg, toxicWaste.gammaReg)
	
	g2PointsAff := curve.BatchScalarMultiplicationG2(&g2, g2Scalars)

	pk.G2.B = g2PointsAff[:nbWires]

	// sets pk: [β]2, [δ]2
	pk.G2.Beta = g2PointsAff[nbWires+0]
	pk.G2.Delta = g2PointsAff[nbWires+1]

	// sets vk: -[δ]2, -[γ]2
	vk.G2.DeltaNeg = g2PointsAff[nbWires+1]
	vk.G2.GammaNeg = g2PointsAff[nbWires+2]
	vk.G2.DeltaNeg.Neg(&vk.G2.DeltaNeg)
	vk.G2.GammaNeg.Neg(&vk.G2.GammaNeg)


	// ---------------------------------------------------------------------------------------------
	// Pairing: vk.E
	vk.E = curve.FinalExponentiation(curve.MillerLoop(pk.G1.Alpha, pk.G2.Beta))
	
	// set domain
	pk.Domain = *domain
}

func setupABC(r1cs *{{toLower .Curve}}backend.R1CS, g *fft.Domain, toxicWaste toxicWaste) (A []fr.Element, B []fr.Element, C []fr.Element) {

	nbWires := r1cs.NbWires

	A = make([]fr.Element, nbWires)
	B = make([]fr.Element, nbWires)
	C = make([]fr.Element, nbWires)

	var one fr.Element
	one.SetOne()

	// evaluation of the i-th lagrange polynomial at t
	var ithLagrangePolt fr.Element

	// L0 = 1/n*(t^n-1)/(t-1), Li+1 = w*Li*(t-w^i)/(t-w^(i+1))
	var w, wi, tmp fr.Element
	w.Set(&g.Generator)
	wi.SetOne()

	// Setting L0
	ithLagrangePolt.Set(&toxicWaste.t)
	ithLagrangePolt.Exp(ithLagrangePolt, new(big.Int).SetUint64(uint64(g.Cardinality))).
		Sub(&ithLagrangePolt, &one)
	tmp.Set(&toxicWaste.t).Sub(&tmp, &one)
	ithLagrangePolt.Div(&ithLagrangePolt, &tmp).
		Mul(&ithLagrangePolt, &g.CardinalityInv)

	// Constraints
	for _, c := range r1cs.Constraints {

		for _, t := range c.L {
			r1cs.AddTerm(&A[t.ConstraintID()], t, ithLagrangePolt)
		}
		for _, t := range c.R {
			r1cs.AddTerm(&B[t.ConstraintID()], t, ithLagrangePolt)
		}
		for _, t := range c.O {
			r1cs.AddTerm(&C[t.ConstraintID()], t, ithLagrangePolt)
		}

		// Li+1 = w*Li*(t-w^i)/(t-w^(i+1))
		ithLagrangePolt.MulAssign(&w)
		tmp.Sub(&toxicWaste.t, &wi)
		ithLagrangePolt.MulAssign(&tmp)
		wi.MulAssign(&w)
		tmp.Sub(&toxicWaste.t, &wi)
		ithLagrangePolt.Div(&ithLagrangePolt, &tmp)
	}
	return

}


// toxicWaste toxic waste
type toxicWaste struct {

	// Montgomery form of params
	t, alpha, beta, gamma, delta fr.Element

	// Non Montgomery form of params
	alphaReg, betaReg, gammaReg, deltaReg fr.Element
}

func sampleToxicWaste() toxicWaste {

	res := toxicWaste{}

	res.t.SetRandom()
	res.alpha.SetRandom()
	res.beta.SetRandom()
	res.gamma.SetRandom()
	res.delta.SetRandom()

	res.alphaReg = res.alpha.ToRegular()
	res.betaReg = res.beta.ToRegular()
	res.gammaReg = res.gamma.ToRegular()
	res.deltaReg = res.delta.ToRegular()

	return res
}



// DummySetup fills a random ProvingKey
// used for test or benchmarking purposes
func DummySetup(r1cs *{{toLower .Curve}}backend.R1CS, pk *ProvingKey) {
	// get R1CS nb constraints, wires and public/private inputs
	nbWires := r1cs.NbWires
	nbConstraints := r1cs.NbConstraints

	// Setting group for fft
	domain := fft.NewDomain(nbConstraints)

	// initialize proving key
	pk.G1.A = make([]curve.G1Affine, nbWires)
	pk.G1.B = make([]curve.G1Affine, nbWires)
	pk.G1.K = make([]curve.G1Affine, r1cs.NbWires-r1cs.NbPublicWires)
	pk.G1.Z = make([]curve.G1Affine, domain.Cardinality)
	pk.G2.B = make([]curve.G2Affine, nbWires)

	// samples toxic waste
	toxicWaste := sampleToxicWaste()

	
	var r1Jac curve.G1Jac
	var r1Aff curve.G1Affine
	var b big.Int
	g1, g2, _, _ := curve.Generators()
	r1Jac.ScalarMultiplication(&g1, toxicWaste.alphaReg.ToBigInt(&b))
	r1Aff.FromJacobian(&r1Jac)
	var r2Jac curve.G2Jac
	var r2Aff curve.G2Affine
	r2Jac.ScalarMultiplication(&g2, &b)
	r2Aff.FromJacobian(&r2Jac)
	for i := 0; i < nbWires; i++ {
		pk.G1.A[i] = r1Aff
		pk.G1.B[i] = r1Aff
		pk.G2.B[i] = r2Aff
	}
	for i := 0; i < len(pk.G1.Z); i++ {
		pk.G1.Z[i] = r1Aff
	}
	for i := 0; i < len(pk.G1.K); i++ {
		pk.G1.K[i] = r1Aff
	}
	pk.G1.Alpha = r1Aff
	pk.G1.Beta = r1Aff
	pk.G1.Delta = r1Aff
	pk.G2.Beta = r2Aff
	pk.G2.Delta = r2Aff

	pk.Domain = *domain

}


// IsDifferent returns true if provided vk is different than self
// this is used by groth16.Assert to ensure random sampling
func (vk *VerifyingKey) IsDifferent(_other interface{}) bool {
	vk2 := _other.(*VerifyingKey)
	for i := 0; i < len(vk.G1.K); i++ {
		if !vk.G1.K[i].IsInfinity() {
			if vk.G1.K[i].Equal(&vk2.G1.K[i]) {
				return false
			}
		}
	}

	return true
}

// IsDifferent returns true if provided pk is different than self
// this is used by groth16.Assert to ensure random sampling
func (pk *ProvingKey) IsDifferent(_other interface{}) bool {
	pk2 := _other.(*ProvingKey)

	if pk.G1.Alpha.Equal(&pk2.G1.Alpha) ||
	pk.G1.Beta.Equal(&pk2.G1.Beta) || 
	pk.G1.Delta.Equal(&pk2.G1.Delta) {
		return false 
	}

	for i := 0; i < len(pk.G1.K); i++ {
		if !pk.G1.K[i].IsInfinity() {
			if pk.G1.K[i].Equal(&pk2.G1.K[i]) {
				return false
			}
		}
	}

	return true
}


// GetCurveID returns the curveID
func (pk *ProvingKey) GetCurveID() gurvy.ID {
	return curve.ID
}

// GetCurveID returns the curveID
func (vk *VerifyingKey) GetCurveID() gurvy.ID {
	return curve.ID
}



// bitRerverse permutation as in fft.BitReverse , but with []curve.G1Affine
func bitReverse(a []curve.G1Affine) {
	n := uint(len(a))
	nn := uint(bits.UintSize - bits.TrailingZeros(n))

	for i := uint(0); i < n; i++ {
		irev := bits.Reverse(i) >> nn
		if irev > i {
			a[i], a[irev] = a[irev],a[i]
		}
	}
}


`
