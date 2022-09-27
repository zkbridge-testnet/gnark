// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by gnark DO NOT EDIT

package cs

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"

	"encoding/binary"
	"unsafe"

	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/hint"
	"github.com/consensys/gnark/backend/witness"
	"github.com/consensys/gnark/frontend/compiled"
	"github.com/consensys/gnark/frontend/schema"
	"github.com/consensys/gnark/internal/backend/ioutils"
	"github.com/consensys/gnark/logger"

	"math"

	"github.com/consensys/gnark-crypto/ecc"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	bn254witness "github.com/consensys/gnark/internal/backend/bn254/witness"
)

// R1CS describes a set of R1CS constraint
type R1CS struct {
	compiled.R1CS
	Coefficients []fr.Element // R1C coefficients indexes point here
}

// NewR1CS returns a new R1CS and sets cs.Coefficient (fr.Element) from provided big.Int values
func NewR1CS(cs compiled.R1CS, coefficients []big.Int) *R1CS {
	r := R1CS{
		R1CS:         cs,
		Coefficients: make([]fr.Element, len(coefficients)),
	}
	for i := 0; i < len(coefficients); i++ {
		r.Coefficients[i].SetBigInt(&coefficients[i])
	}

	return &r
}

// Solve sets all the wires and returns the a, b, c vectors.
// the cs system should have been compiled before. The entries in a, b, c are in Montgomery form.
// a, b, c vectors: ab-c = hz
// witness = [publicWires | secretWires] (without the ONE_WIRE !)
// returns  [publicWires | secretWires | internalWires ]
func (cs *R1CS) Solve(witness, a, b, c []fr.Element, opt backend.ProverConfig) ([]fr.Element, error) {
	log := logger.Logger().With().Str("curve", cs.CurveID().String()).Int("nbConstraints", len(cs.Constraints)).Str("backend", "groth16").Logger()

	nbWires := cs.NbPublicVariables + cs.NbSecretVariables + cs.NbInternalVariables
	solution, err := newSolution(nbWires, opt.HintFunctions, cs.MHintsDependencies, cs.MHints, cs.Coefficients)
	if err != nil {
		return make([]fr.Element, nbWires), err
	}
	start := time.Now()

	if len(witness) != int(cs.NbPublicVariables-1+cs.NbSecretVariables) { // - 1 for ONE_WIRE
		err = fmt.Errorf("invalid witness size, got %d, expected %d = %d (public) + %d (secret)", len(witness), int(cs.NbPublicVariables-1+cs.NbSecretVariables), cs.NbPublicVariables-1, cs.NbSecretVariables)
		log.Err(err).Send()
		return solution.values, err
	}

	// compute the wires and the a, b, c polynomials
	if len(a) != len(cs.Constraints) || len(b) != len(cs.Constraints) || len(c) != len(cs.Constraints) {
		err = errors.New("invalid input size: len(a, b, c) == len(Constraints)")
		log.Err(err).Send()
		return solution.values, err
	}

	solution.solved[0] = true // ONE_WIRE
	solution.values[0].SetOne()
	copy(solution.values[1:], witness)
	for i := 0; i < len(witness); i++ {
		solution.solved[i+1] = true
	}

	// keep track of the number of wire instantiations we do, for a sanity check to ensure
	// we instantiated all wires
	solution.nbSolved += uint64(len(witness) + 1)

	// now that we know all inputs are set, defer log printing once all solution.values are computed
	// (or sooner, if a constraint is not satisfied)
	defer solution.printLogs(opt.CircuitLogger, cs.Logs)

	if err := cs.parallelSolve(a, b, c, &solution); err != nil {
		if unsatisfiedErr, ok := err.(*UnsatisfiedConstraintError); ok {
			log.Err(errors.New("unsatisfied constraint")).Int("id", unsatisfiedErr.CID).Send()
		} else {
			log.Err(err).Send()
		}
		return solution.values, err
	}

	// sanity check; ensure all wires are marked as "instantiated"
	if !solution.isValid() {
		log.Err(errors.New("solver didn't instantiate all wires")).Send()
		panic("solver didn't instantiate all wires")
	}

	log.Debug().Dur("took", time.Since(start)).Msg("constraint system solver done")

	return solution.values, nil
}

func (cs *R1CS) parallelSolve(a, b, c []fr.Element, solution *solution) error {
	// minWorkPerCPU is the minimum target number of constraint a task should hold
	// in other words, if a level has less than minWorkPerCPU, it will not be parallelized and executed
	// sequentially without sync.
	const minWorkPerCPU = 50.0

	// cs.Levels has a list of levels, where all constraints in a level l(n) are independent
	// and may only have dependencies on previous levels
	// for each constraint
	// we are guaranteed that each R1C contains at most one unsolved wire
	// first we solve the unsolved wire (if any)
	// then we check that the constraint is valid
	// if a[i] * b[i] != c[i]; it means the constraint is not satisfied

	var wg sync.WaitGroup
	chTasks := make(chan []int, runtime.NumCPU())
	chError := make(chan *UnsatisfiedConstraintError, runtime.NumCPU())

	// start a worker pool
	// each worker wait on chTasks
	// a task is a slice of constraint indexes to be solved
	for i := 0; i < runtime.NumCPU(); i++ {
		go func() {
			for t := range chTasks {
				for _, i := range t {
					// for each constraint in the task, solve it.
					if err := cs.solveConstraint(cs.Constraints[i], solution, &a[i], &b[i], &c[i]); err != nil {
						var debugInfo *string
						if dID, ok := cs.MDebug[int(i)]; ok {
							debugInfo = new(string)
							*debugInfo = solution.logValue(cs.DebugInfo[dID])
						}
						chError <- &UnsatisfiedConstraintError{CID: i, Err: err, DebugInfo: debugInfo}
						wg.Done()
						return
					}
				}
				wg.Done()
			}
		}()
	}

	// clean up pool go routines
	defer func() {
		close(chTasks)
		close(chError)
	}()

	// for each level, we push the tasks
	for _, level := range cs.Levels {

		// max CPU to use
		maxCPU := float64(len(level)) / minWorkPerCPU

		if maxCPU <= 1.0 {
			// we do it sequentially
			for _, i := range level {
				if err := cs.solveConstraint(cs.Constraints[i], solution, &a[i], &b[i], &c[i]); err != nil {
					var debugInfo *string
					if dID, ok := cs.MDebug[int(i)]; ok {
						debugInfo = new(string)
						*debugInfo = solution.logValue(cs.DebugInfo[dID])
					}
					return &UnsatisfiedConstraintError{CID: i, Err: err, DebugInfo: debugInfo}
				}
			}
			continue
		}

		// number of tasks for this level is set to num cpus
		// but if we don't have enough work for all our CPUS, it can be lower.
		nbTasks := runtime.NumCPU()
		maxTasks := int(math.Ceil(maxCPU))
		if nbTasks > maxTasks {
			nbTasks = maxTasks
		}
		nbIterationsPerCpus := len(level) / nbTasks

		// more CPUs than tasks: a CPU will work on exactly one iteration
		// note: this depends on minWorkPerCPU constant
		if nbIterationsPerCpus < 1 {
			nbIterationsPerCpus = 1
			nbTasks = len(level)
		}

		extraTasks := len(level) - (nbTasks * nbIterationsPerCpus)
		extraTasksOffset := 0

		for i := 0; i < nbTasks; i++ {
			wg.Add(1)
			_start := i*nbIterationsPerCpus + extraTasksOffset
			_end := _start + nbIterationsPerCpus
			if extraTasks > 0 {
				_end++
				extraTasks--
				extraTasksOffset++
			}
			// since we're never pushing more than num CPU tasks
			// we will never be blocked here
			chTasks <- level[_start:_end]
		}

		// wait for the level to be done
		wg.Wait()

		if len(chError) > 0 {
			return <-chError
		}
	}

	return nil
}

// IsSolved returns nil if given witness solves the R1CS and error otherwise
// this method wraps cs.Solve() and allocates cs.Solve() inputs
func (cs *R1CS) IsSolved(witness *witness.Witness, opts ...backend.ProverOption) error {
	opt, err := backend.NewProverConfig(opts...)
	if err != nil {
		return err
	}

	a := make([]fr.Element, len(cs.Constraints))
	b := make([]fr.Element, len(cs.Constraints))
	c := make([]fr.Element, len(cs.Constraints))
	v := witness.Vector.(*bn254witness.Witness)
	_, err = cs.Solve(*v, a, b, c, opt)
	return err
}

// divByCoeff sets res = res / t.Coeff
func (cs *R1CS) divByCoeff(res *fr.Element, t compiled.Term) {
	cID := t.CoeffID()
	switch cID {
	case compiled.CoeffIdOne:
		return
	case compiled.CoeffIdMinusOne:
		res.Neg(res)
	case compiled.CoeffIdZero:
		panic("division by 0")
	default:
		// this is slow, but shouldn't happen as divByCoeff is called to
		// remove the coeff of an unsolved wire
		// but unsolved wires are (in gnark frontend) systematically set with a coeff == 1 or -1
		res.Div(res, &cs.Coefficients[cID])
	}
}

// solveConstraint compute unsolved wires in the constraint, if any and set the solution accordingly
//
// returns an error if the solver called a hint function that errored
// returns false, nil if there was no wire to solve
// returns true, nil if exactly one wire was solved. In that case, it is redundant to check that
// the constraint is satisfied later.
func (cs *R1CS) solveConstraint(r compiled.R1C, solution *solution, a, b, c *fr.Element) error {

	// the index of the non zero entry shows if L, R or O has an uninstantiated wire
	// the content is the ID of the wire non instantiated
	var loc uint8

	var termToCompute compiled.Term

	processLExp := func(l compiled.LinearExpression, val *fr.Element, locValue uint8) error {
		for _, t := range l {
			vID := t.WireID()

			// wire is already computed, we just accumulate in val
			if solution.solved[vID] {
				solution.accumulateInto(t, val)
				continue
			}

			// first we check if this is a hint wire
			if hint, ok := cs.MHints[vID]; ok {
				if err := solution.solveWithHint(vID, hint); err != nil {
					return err
				}
				// now that the wire is saved, accumulate it into a, b or c
				solution.accumulateInto(t, val)
				continue
			}

			if loc != 0 {
				panic("found more than one wire to instantiate")
			}
			termToCompute = t
			loc = locValue
		}
		return nil
	}

	if err := processLExp(r.L, a, 1); err != nil {
		return err
	}

	if err := processLExp(r.R, b, 2); err != nil {
		return err
	}

	if err := processLExp(r.O, c, 3); err != nil {
		return err
	}

	if loc == 0 {
		// there is nothing to solve, may happen if we have an assertion
		// (ie a constraints that doesn't yield any output)
		// or if we solved the unsolved wires with hint functions
		var check fr.Element
		if !check.Mul(a, b).Equal(c) {
			return fmt.Errorf("%s ⋅ %s != %s", a.String(), b.String(), c.String())
		}
		return nil
	}

	// we compute the wire value and instantiate it
	wID := termToCompute.WireID()

	// solver result
	var wire fr.Element

	switch loc {
	case 1:
		if !b.IsZero() {
			wire.Div(c, b).
				Sub(&wire, a)
			a.Add(a, &wire)
		} else {
			// we didn't actually ensure that a * b == c
			var check fr.Element
			if !check.Mul(a, b).Equal(c) {
				return fmt.Errorf("%s ⋅ %s != %s", a.String(), b.String(), c.String())
			}
		}
	case 2:
		if !a.IsZero() {
			wire.Div(c, a).
				Sub(&wire, b)
			b.Add(b, &wire)
		} else {
			var check fr.Element
			if !check.Mul(a, b).Equal(c) {
				return fmt.Errorf("%s ⋅ %s != %s", a.String(), b.String(), c.String())
			}
		}
	case 3:
		wire.Mul(a, b).
			Sub(&wire, c)

		c.Add(c, &wire)
	}

	// wire is the term (coeff * value)
	// but in the solution we want to store the value only
	// note that in gnark frontend, coeff here is always 1 or -1
	cs.divByCoeff(&wire, termToCompute)
	solution.set(wID, wire)

	return nil
}

// GetConstraints return a list of constraint formatted as L⋅R == O
// such that [0] -> L, [1] -> R, [2] -> O
func (cs *R1CS) GetConstraints() [][]string {
	r := make([][]string, 0, len(cs.Constraints))
	for _, c := range cs.Constraints {
		// for each constraint, we build a string representation of it's L, R and O part
		// if we are worried about perf for large cs, we could do a string builder + csv format.
		var line [3]string
		line[0] = cs.vtoString(c.L)
		line[1] = cs.vtoString(c.R)
		line[2] = cs.vtoString(c.O)
		r = append(r, line[:])
	}
	return r
}

func (cs *R1CS) vtoString(l compiled.LinearExpression) string {
	var sbb strings.Builder
	for i := 0; i < len(l); i++ {
		cs.termToString(l[i], &sbb)
		if i+1 < len(l) {
			sbb.WriteString(" + ")
		}
	}
	return sbb.String()
}

func (cs *R1CS) termToString(t compiled.Term, sbb *strings.Builder) {
	tID := t.CoeffID()
	if tID == compiled.CoeffIdOne {
		// do nothing, just print the variable
	} else if tID == compiled.CoeffIdMinusOne {
		// print neg sign
		sbb.WriteByte('-')
	} else if tID == compiled.CoeffIdZero {
		sbb.WriteByte('0')
		return
	} else {
		sbb.WriteString(cs.Coefficients[tID].String())
		sbb.WriteString("⋅")
	}
	vID := t.WireID()
	visibility := t.VariableVisibility()

	switch visibility {
	case schema.Internal:
		if _, isHint := cs.MHints[vID]; isHint {
			sbb.WriteString(fmt.Sprintf("hv%d", vID-cs.NbPublicVariables-cs.NbSecretVariables))
		} else {
			sbb.WriteString(fmt.Sprintf("v%d", vID-cs.NbPublicVariables-cs.NbSecretVariables))
		}
	case schema.Public:
		if vID == 0 {
			sbb.WriteByte('1') // one wire
		} else {
			sbb.WriteString(fmt.Sprintf("p%d", vID-1))
		}
	case schema.Secret:
		sbb.WriteString(fmt.Sprintf("s%d", vID-cs.NbPublicVariables))
	default:
		sbb.WriteString("<?>")
	}
}

// GetNbCoefficients return the number of unique coefficients needed in the R1CS
func (cs *R1CS) GetNbCoefficients() int {
	return len(cs.Coefficients)
}

// CurveID returns curve ID as defined in gnark-crypto
func (cs *R1CS) CurveID() ecc.ID {
	return ecc.BN254
}

// FrSize return fr.Limbs * 8, size in byte of a fr element
func (cs *R1CS) FrSize() int {
	return fr.Limbs * 8
}

func int_to_byte(x int) []byte {
	var b [8]byte
	if unsafe.Sizeof(x) == 8 {
		binary.LittleEndian.PutUint64(b[:], uint64(x))
	} else {
		panic("unknown int size")
	}
	return b[:]
}

func uint64_to_byte(x uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], x)
	return b[:]
}

func uint32_to_byte(x uint32) []byte {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], x)
	return buf[:]
}

func hintID_to_byte(x hint.ID) []byte {
	//hint.ID is uint32
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(x))
	return buf[:]
}

// WriteTo encodes R1CS into provided io.Writer using cbor
func (cs *R1CS) WriteTo(w io.Writer) (int64, error) {
	//Naive array to binary
	fmt.Println("Writing R1CS to file")

	err := encodeMHintsToWriter(w, cs.MHints)
	if err != nil {
		return 0, err
	}
	err = encodeConstraintsToWriter(w, cs.Constraints)
	if err != nil {
		return 0, err
	}

	_w := ioutils.WriterCounter{W: w} // wraps writer to count the bytes written
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		return 0, err
	}
	encoder := enc.NewEncoder(&_w)

	fmt.Printf("MHints len: %v\n", len(cs.MHints))

	// encode our object
	//err = encoder.Encode(cs)

	t0 := time.Now()
	err = encoder.Encode(cs.ConstraintSystem.Schema)
	if err != nil {
		return _w.N, err
	}
	t1 := time.Now()
	fmt.Printf("Encoding Schema took: %0.2fs\n", t1.Sub(t0).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.NbInternalVariables)
	if err != nil {
		return _w.N, err
	}
	t2 := time.Now()
	fmt.Printf("Encoding NbInternalVariables took: %0.2fs\n", t2.Sub(t1).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.NbPublicVariables)
	if err != nil {
		return _w.N, err
	}

	t3 := time.Now()
	fmt.Printf("Encoding NbPublicVariables took: %0.2fs\n", t3.Sub(t2).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.NbSecretVariables)
	if err != nil {
		return _w.N, err
	}

	t4 := time.Now()
	fmt.Printf("Encoding NbSecretVariables took: %0.2fs\n", t4.Sub(t3).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.Public)
	if err != nil {
		return _w.N, err
	}

	t5 := time.Now()
	fmt.Printf("Encoding Public took: %0.2fs\n", t5.Sub(t4).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.Secret)
	if err != nil {
		return _w.N, err
	}

	t6 := time.Now()
	fmt.Printf("Encoding Secret took: %0.2fs\n", t6.Sub(t5).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.Logs)
	if err != nil {
		return _w.N, err
	}
	t7 := time.Now()
	fmt.Printf("Encoding Logs took: %0.2fs\n", t7.Sub(t6).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.DebugInfo)
	if err != nil {
		return _w.N, err
	}

	t8 := time.Now()
	fmt.Printf("Encoding DebugInfo took: %0.2fs\n", t8.Sub(t7).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.MDebug)
	if err != nil {
		return _w.N, err
	}

	t9 := time.Now()
	fmt.Printf("Encoding MDebug took: %0.2fs\n", t9.Sub(t8).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.Counters)
	if err != nil {
		return _w.N, err
	}
	t10 := time.Now()
	fmt.Printf("Encoding Counters took: %0.2fs\n", t10.Sub(t9).Seconds())

	//err = encoder.Encode(cs.ConstraintSystem.MHints)
	//if err != nil {
	//	return _w.N, err
	//}
	//fmt.Printf("Encoding MHints took: %0.2fs\n", t11.Sub(t10).Seconds())
	t11 := time.Now()
	err = encoder.Encode(cs.ConstraintSystem.MHintsDependencies)
	if err != nil {
		return _w.N, err
	}
	t12 := time.Now()
	fmt.Printf("Encoding MHintsDependencies took: %0.2fs\n", t12.Sub(t11).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.Levels)
	if err != nil {
		return _w.N, err
	}
	t13 := time.Now()
	fmt.Printf("Encoding Levels took: %0.2fs\n", t13.Sub(t12).Seconds())

	err = encoder.Encode(cs.ConstraintSystem.CurveID)
	if err != nil {
		return _w.N, err
	}

	t14 := time.Now()
	fmt.Printf("Encoding CurveID took: %0.2fs\n", t14.Sub(t13).Seconds())

	//err = encoder.Encode(cs.Constraints)
	//if err != nil {
	//	return _w.N, err
	//}

	t15 := time.Now()
	//fmt.Printf("Encoding Constraints took: %0.2fs\n", t15.Sub(t14).Seconds())

	err = encoder.Encode(cs.Coefficients)

	t16 := time.Now()
	fmt.Printf("Encoding Coefficients took: %0.2fs\n", t16.Sub(t15).Seconds())

	return _w.N, err
}

func byte_to_int(b []byte, offset int) (int, int) {
	result_uint64 := binary.LittleEndian.Uint64(b[offset : offset+8])
	result := int(result_uint64)
	return result, 8
}

func byte_to_hintID(b []byte, offset int) (hint.ID, int) {
	result_uint32 := binary.LittleEndian.Uint32(b[offset : offset+4])
	result := hint.ID(result_uint32)
	return result, 4
}

func byte_to_uint64(b []byte, offset int) (uint64, int) {
	result := binary.LittleEndian.Uint64(b[offset : offset+8])
	return result, 8
}

// ReadFrom attempts to decode R1CS from io.Reader using cbor
func (cs *R1CS) ReadFrom(r io.Reader) (int64, error) {
	dm, err := cbor.DecOptions{
		MaxArrayElements: 134217728,
		MaxMapPairs:      134217728,
	}.DecMode()

	if err != nil {
		return 0, err
	}
	//start := time.Now()
	//fmt.Printf("Decoder Created took: %0.2fs\n", time.Now().Sub(start).Seconds())
	//if err := decoder.Decode(&cs); err != nil {
	//	return int64(decoder.NumBytesRead()), err
	//}

	cs.ConstraintSystem.Schema = &schema.Schema{}
	cs.ConstraintSystem.MHints, err = decodeMHintsFromReader(r)
	if err != nil {
		return 0, err
	}
	cs.Constraints, err = decodeConstraintsFromReader(r)
	if err != nil {
		return 0, err
	}

	decoder := dm.NewDecoder(r)
	t0 := time.Now()
	err = decoder.Decode(cs.ConstraintSystem.Schema)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t1 := time.Now()
	fmt.Printf("Decoding Schema took: %0.2fs\n", t1.Sub(t0).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.NbInternalVariables)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t2 := time.Now()
	fmt.Printf("Decoding NbInternalVariables took: %0.2fs\n", t2.Sub(t1).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.NbPublicVariables)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t3 := time.Now()
	fmt.Printf("Decoding NbPublicVariables took: %0.2fs\n", t3.Sub(t2).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.NbSecretVariables)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t4 := time.Now()
	fmt.Printf("Decoding NbSecretVariables took: %0.2fs\n", t4.Sub(t3).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.Public)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t5 := time.Now()
	fmt.Printf("Decoding Public took: %0.2fs\n", t5.Sub(t4).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.Secret)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t6 := time.Now()
	fmt.Printf("Decoding Secret took: %0.2fs\n", t6.Sub(t5).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.Logs)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t7 := time.Now()
	fmt.Printf("Decoding Logs took: %0.2fs\n", t7.Sub(t6).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.DebugInfo)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t8 := time.Now()
	fmt.Printf("Decoding DebugInfo took: %0.2fs\n", t8.Sub(t7).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.MDebug)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t9 := time.Now()
	fmt.Printf("Decoding MDebug took: %0.2fs\n", t9.Sub(t8).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.Counters)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t10 := time.Now()
	fmt.Printf("Decoding Counters took: %0.2fs\n", t10.Sub(t9).Seconds())

	t11 := time.Now()
	//fmt.Printf("Decoding MHints took: %0.2fs\n", t11.Sub(t10).Seconds())
	err = decoder.Decode(&cs.ConstraintSystem.MHintsDependencies)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t12 := time.Now()
	fmt.Printf("Decoding MHintsDependencies took: %0.2fs\n", t12.Sub(t11).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.Levels)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t13 := time.Now()
	fmt.Printf("Decoding Levels took: %0.2fs\n", t13.Sub(t12).Seconds())

	err = decoder.Decode(&cs.ConstraintSystem.CurveID)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t14 := time.Now()
	fmt.Printf("Decoding CurveID took: %0.2fs\n", t14.Sub(t13).Seconds())

	//err = decoder.Decode(&cs.Constraints)
	//if err != nil {
	//	return int64(decoder.NumBytesRead()), err
	//}

	t15 := time.Now()
	//fmt.Printf("Decoding Constraints took: %0.2fs\n", t15.Sub(t14).Seconds())

	err = decoder.Decode(&cs.Coefficients)
	if err != nil {
		return int64(decoder.NumBytesRead()), err
	}
	t16 := time.Now()
	fmt.Printf("Decoding Coefficients took: %0.2fs\n", t16.Sub(t15).Seconds())

	fmt.Printf("MHints len: %v\n", len(cs.MHints))

	return int64(decoder.NumBytesRead()), nil
}

func encodeMHintsToWriter(w io.Writer, mhints map[int]*compiled.Hint) error {
	start := time.Now()
	defer func() {
		fmt.Printf("Encoding MHints done, took %0.2fs\n", time.Since(start).Seconds())
	}()

	mhintsBinary := encodeMHints(mhints)

	_, err := w.Write(uint64_to_byte(uint64(len(mhintsBinary))))
	if err != nil {
		return err
	}
	_, err = w.Write(mhintsBinary)
	if err != nil {
		return err
	}
	return nil
}

func encodeMHints(mhints map[int]*compiled.Hint) []byte {
	mhintsBinary := make([]byte, 0)

	mhintsBinary = append(mhintsBinary, int_to_byte(len(mhints))...)

	mhintscheck := make(map[*compiled.Hint]int)

	for k, v := range mhints {
		mhintsBinary = append(mhintsBinary, int_to_byte(k)...)
		if lastKey, ok := mhintscheck[v]; ok {
			//do something here
			mhintsBinary = append(mhintsBinary, int_to_byte(0)...)
			mhintsBinary = append(mhintsBinary, int_to_byte(lastKey)...)
			continue
		} else {
			mhintsBinary = append(mhintsBinary, int_to_byte(1)...)
		}
		mhintscheck[v] = k
		//Serialize a hint
		hintBinary := make([]byte, 0)
		hintBinary = append(hintBinary, hintID_to_byte(v.ID)...)
		//convert array of int to array of byte
		hintBinary = append(hintBinary, int_to_byte(len(v.Wires))...)
		for i := 0; i < len(v.Wires); i++ {
			hintBinary = append(hintBinary, int_to_byte(v.Wires[i])...)
		}
		hintBinary = append(hintBinary, int_to_byte(len(v.Inputs))...)
		for i := 0; i < len(v.Inputs); i++ {
			switch vit := v.Inputs[i].(type) {
			case big.Int:
				hintBinary = append(hintBinary, int_to_byte(int(25446))...)
				hintBinary = append(hintBinary, int_to_byte(len(vit.Bytes()))...)
				hintBinary = append(hintBinary, vit.Bytes()...)
			case *big.Int:
				hintBinary = append(hintBinary, int_to_byte(int(25447))...)
				hintBinary = append(hintBinary, int_to_byte(len(vit.Bytes()))...)
				hintBinary = append(hintBinary, vit.Bytes()...)
			case compiled.LinearExpression:
				//linear expression = []Term = []Uint64
				hintBinary = append(hintBinary, int_to_byte(int(25443))...)
				hintBinary = append(hintBinary, int_to_byte(len(vit))...)
				for j := 0; j < len(vit); j++ {
					hintBinary = append(hintBinary, uint64_to_byte(uint64(vit[j]))...)
				}
			default:
				fmt.Println(reflect.TypeOf(vit))
				panic("unknown type")
			}
		}
		mhintsBinary = append(mhintsBinary, hintBinary...)
	}
	return mhintsBinary
}

func decodeMHintsFromReader(r io.Reader) (map[int]*compiled.Hint, error) {
	t0 := time.Now()
	defer func() {
		fmt.Printf("Decoding MHints took: %0.2fs\n", time.Now().Sub(t0).Seconds())
	}()

	var mHintLenBytes [8]byte
	_, err := r.Read(mHintLenBytes[:])
	if err != nil {
		return nil, err
	}
	mHintLen := binary.LittleEndian.Uint64(mHintLenBytes[:])
	mHintBytes, err := ioutils.Read(r, int(mHintLen))
	if err != nil {
		return nil, err
	}
	return decodeMHints(mHintBytes)
}

func decodeMHints(mHintBytes []byte) (map[int]*compiled.Hint, error) {

	//start decode hint
	hint := make(map[int]*compiled.Hint)
	bytes_used := 0
	lenHint, offset := byte_to_int(mHintBytes, bytes_used)
	bytes_used += offset
	for i := 0; i < lenHint; i++ {
		k, offset := byte_to_int(mHintBytes, bytes_used)
		bytes_used += offset
		var v compiled.Hint

		mode, offset := byte_to_int(mHintBytes, bytes_used)
		bytes_used += offset
		if mode == 0 {
			lastKey, offset := byte_to_int(mHintBytes, bytes_used)
			bytes_used += offset
			hint[k] = hint[lastKey]
			continue
		} else if mode == 1 {
		} else {
			panic("mode error")
		}

		v.ID, offset = byte_to_hintID(mHintBytes, bytes_used)
		bytes_used += offset

		wireLen, offset := byte_to_int(mHintBytes, bytes_used)
		bytes_used += offset
		v.Wires = make([]int, wireLen)
		for j := 0; j < wireLen; j++ {
			v.Wires[j], offset = byte_to_int(mHintBytes, bytes_used)
			bytes_used += offset
		}
		inputLen, offset := byte_to_int(mHintBytes, bytes_used)
		bytes_used += offset
		v.Inputs = make([]interface{}, inputLen)
		for j := 0; j < inputLen; j++ {
			typeTag, offset := byte_to_int(mHintBytes, bytes_used)
			bytes_used += offset
			switch typeTag {
			case 25446:
				//big.Int
				val := new(big.Int)
				bigIntLen, offset := byte_to_int(mHintBytes, bytes_used)
				bytes_used += offset
				bb := mHintBytes[bytes_used : bytes_used+bigIntLen]
				bytes_used += bigIntLen
				val.SetBytes(bb)
				v.Inputs[j] = *val
			case 25447:
				//*big.Int
				val := new(big.Int)
				bigIntLen, offset := byte_to_int(mHintBytes, bytes_used)
				bytes_used += offset
				bb := mHintBytes[bytes_used : bytes_used+bigIntLen]
				bytes_used += bigIntLen
				val.SetBytes(bb)
				v.Inputs[j] = val
			case 25443:
				//LinearExpression
				linLen, offset := byte_to_int(mHintBytes, bytes_used)
				bytes_used += offset
				val := make([]compiled.Term, linLen)
				for k := 0; k < linLen; k++ {
					uint64v, offset := byte_to_uint64(mHintBytes, bytes_used)
					bytes_used += offset
					val[k] = compiled.Term(uint64v)
				}
				v.Inputs[j] = compiled.LinearExpression(val)
			default:
				panic("typeTag error")
			}
		}
		hint[k] = &v
	}
	return hint, nil
}

func encodeConstraintsToWriter(w io.Writer, constraints []compiled.R1C) error {
	start := time.Now()
	defer func() {
		fmt.Printf("Encoding Constraints done, took %0.2fs\n", time.Since(start).Seconds())
	}()
	mConstraintsBinary := encodeConstraints(constraints)
	_, err := w.Write(uint64_to_byte(uint64(len(mConstraintsBinary))))
	if err != nil {
		return err
	}
	_, err = w.Write(mConstraintsBinary)
	if err != nil {
		return err
	}
	return nil
}

func encodeConstraints(constraints []compiled.R1C) []byte {
	R1CsBytes := uint64_to_byte(uint64(len(constraints)))
	for _, r1c := range constraints {
		R1CsBytes = append(R1CsBytes, encodeLinearExpression(r1c.L)...)
		R1CsBytes = append(R1CsBytes, encodeLinearExpression(r1c.R)...)
		R1CsBytes = append(R1CsBytes, encodeLinearExpression(r1c.O)...)
	}
	return R1CsBytes
}

func encodeLinearExpression(l compiled.LinearExpression) []byte {
	var retBytes []byte
	retBytes = append(retBytes, uint64_to_byte(uint64(len(l)))...)
	for _, term := range l {
		retBytes = append(retBytes, uint64_to_byte(uint64(term))...)
	}
	return retBytes
}

func decodeConstraintsFromReader(r io.Reader) ([]compiled.R1C, error) {
	t0 := time.Now()
	defer func() {
		fmt.Printf("Decoding Constraints took: %0.2fs\n", time.Now().Sub(t0).Seconds())
	}()

	var constraintLenBytes [8]byte
	_, err := r.Read(constraintLenBytes[:])
	if err != nil {
		return nil, err
	}
	constraintLen := binary.LittleEndian.Uint64(constraintLenBytes[:])
	constraintBytes, err := ioutils.Read(r, int(constraintLen))
	if err != nil {
		return nil, err
	}
	return decodeConstraints(constraintBytes), nil
}

func decodeConstraints(bytes []byte) []compiled.R1C {
	n, offset := byte_to_int(bytes, 0)
	r1c := make([]compiled.R1C, n)
	for i := 0; i < n; i++ {
		L, usedBytes := decodeLinearExpression(bytes[offset:])
		offset += usedBytes
		R, usedBytes := decodeLinearExpression(bytes[offset:])
		offset += usedBytes
		O, usedBytes := decodeLinearExpression(bytes[offset:])
		offset += usedBytes

		r1c[i] = compiled.R1C{
			L: L,
			R: R,
			O: O,
		}
	}
	return r1c
}

func decodeLinearExpression(bytes []byte) (compiled.LinearExpression, int) {
	offset := 0
	nTerm, usedN := byte_to_int(bytes, offset)
	offset += usedN
	le := make([]compiled.Term, nTerm)
	for j := 0; j < nTerm; j++ {
		term, usedN := byte_to_int(bytes, offset)
		le[j] = compiled.Term(term)
		offset += usedN
	}
	return le, offset
}
