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

package cs

import (
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/consensys/gnark/backend/hint"
	"github.com/consensys/gnark/frontend/compiled"
	"github.com/consensys/gnark/frontend/schema"
	"github.com/consensys/gnark/internal/utils"
	"github.com/rs/zerolog"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"

	curve "github.com/consensys/gnark-crypto/ecc/bn254"
)

// solution represents elements needed to compute
// a solution to a R1CS or SparseR1CS
type solution struct {
	values, coefficients []fr.Element
	solved               []bool
	nbSolved             uint64
	mHintsFunctions      map[hint.ID]hint.Function // maps hintID to hint function
	mHints               map[int]*compiled.Hint    // maps wireID to hint
}

func newSolution(nbWires int, hintFunctions map[hint.ID]hint.Function, hintsDependencies map[hint.ID]string, mHints map[int]*compiled.Hint, coefficients []fr.Element) (solution, error) {

	s := solution{
		values:          make([]fr.Element, nbWires),
		coefficients:    coefficients,
		solved:          make([]bool, nbWires),
		mHintsFunctions: hintFunctions,
		mHints:          mHints,
	}

	// hintsDependencies is from compile time; it contains the list of hints the solver **needs**
	var missing []string
	for hintUUID, hintID := range hintsDependencies {
		if _, ok := s.mHintsFunctions[hintUUID]; !ok {
			missing = append(missing, hintID)
		}
	}

	if len(missing) > 0 {
		return s, fmt.Errorf("solver missing hint(s): %v", missing)
	}

	return s, nil
}

func (s *solution) set(id int, value fr.Element) {
	if s.solved[id] {
		panic("solving the same wire twice should never happen.")
	}
	s.values[id] = value
	s.solved[id] = true
	atomic.AddUint64(&s.nbSolved, 1)
	// s.nbSolved++
}

func (s *solution) isValid() bool {
	return int(s.nbSolved) == len(s.values)
}

// computeTerm computes coef*variable
func (s *solution) computeTerm(t compiled.Term) fr.Element {
	cID, vID, _ := t.Unpack()
	if cID != 0 && !s.solved[vID] {
		panic("computing a term with an unsolved wire")
	}
	switch cID {
	case compiled.CoeffIdZero:
		return fr.Element{}
	case compiled.CoeffIdOne:
		return s.values[vID]
	case compiled.CoeffIdTwo:
		var res fr.Element
		res.Double(&s.values[vID])
		return res
	case compiled.CoeffIdMinusOne:
		var res fr.Element
		res.Neg(&s.values[vID])
		return res
	default:
		var res fr.Element
		if s.values[vID].IsZero() {
			return s.values[vID]
		}
		if s.values[vID].IsOne() {
			return s.coefficients[cID]
		}
		res.Mul(&s.coefficients[cID], &s.values[vID])
		return res
	}
}

// r += (t.coeff*t.value)
func (s *solution) accumulateInto(t compiled.Term, r *fr.Element) {
	cID := t.CoeffID()
	vID := t.WireID()
	switch cID {
	case compiled.CoeffIdZero:
		return
	case compiled.CoeffIdOne:
		r.Add(r, &s.values[vID])
	case compiled.CoeffIdTwo:
		var res fr.Element
		res.Double(&s.values[vID])
		r.Add(r, &res)
	case compiled.CoeffIdMinusOne:
		r.Sub(r, &s.values[vID])
	default:
		var res fr.Element
		if s.values[vID].IsZero() {
			return
		}
		if s.values[vID].IsOne() {
			r.Add(r, &s.coefficients[cID])
			return
		}
		res.Mul(&s.coefficients[cID], &s.values[vID])
		r.Add(r, &res)
	}
}

// solveHint compute solution.values[vID] using provided solver hint
func (s *solution) solveWithHint(vID int, h *compiled.Hint) error {
	// skip if the wire is already solved by a call to the same hint
	// function on the same inputs
	if s.solved[vID] {
		return nil
	}
	// ensure hint function was provided
	f, ok := s.mHintsFunctions[h.ID]
	if !ok {
		return errors.New("missing hint function")
	}

	// tmp IO big int memory
	nbInputs := len(h.Inputs)
	nbOutputs := len(h.Wires)
	inputs := make([]*big.Int, nbInputs)
	outputs := make([]*big.Int, nbOutputs)
	for i := 0; i < nbOutputs; i++ {
		outputs[i] = big.NewInt(0)
	}

	q := fr.Modulus()

	// for each input, we set it's big int value, IF all the wires are solved
	// the only case where all wires may not be solved, is if one of the input of this hint
	// is the output of another hint.
	// it is safe to recursively solve this with the parallel solver, since all hints-output wires
	// that we can solve this way are marked to be solved with the current constraint we are processing.
	recursiveSolve := func(t compiled.Term) error {
		wID := t.WireID()
		if s.solved[wID] {
			return nil
		}
		// unsolved dependency
		if h, ok := s.mHints[wID]; ok {
			// solve recursively.
			return s.solveWithHint(wID, h)
		}

		// it's not a hint, we panic.
		panic("solver can't compute hint; one or more input wires are unsolved")
	}

	for i := 0; i < nbInputs; i++ {
		inputs[i] = big.NewInt(0)

		switch t := h.Inputs[i].(type) {
		case compiled.LinearExpression:
			var v fr.Element
			for _, term := range t {
				if err := recursiveSolve(term); err != nil {
					return err
				}
				s.accumulateInto(term, &v)
			}
			v.ToBigIntRegular(inputs[i])
		case compiled.Term:
			if err := recursiveSolve(t); err != nil {
				return err
			}
			v := s.computeTerm(t)
			v.ToBigIntRegular(inputs[i])
		default:
			v := utils.FromInterface(t)
			inputs[i] = &v

			// here we have no guarantee that v < q, so we mod reduce
			inputs[i].Mod(inputs[i], q)
		}
	}

	err := f(curve.ID, inputs, outputs)

	var v fr.Element
	for i := range outputs {
		v.SetBigInt(outputs[i])
		s.set(h.Wires[i], v)
	}

	return err
}

func (s *solution) printLogs(log zerolog.Logger, logs []compiled.LogEntry) {
	if log.GetLevel() == zerolog.Disabled {
		return
	}

	for i := 0; i < len(logs); i++ {
		logLine := s.logValue(logs[i])
		log.Debug().Str(zerolog.CallerFieldName, logs[i].Caller).Msg(logLine)
	}
}

const unsolvedVariable = "<unsolved>"

func (s *solution) logValue(log compiled.LogEntry) string {
	var toResolve []interface{}
	var (
		isEval       bool
		eval         fr.Element
		missingValue bool
	)
	for j := 0; j < len(log.ToResolve); j++ {
		if log.ToResolve[j] == compiled.TermDelimitor {
			// this is a special case where we want to evaluate the following terms until the next delimitor.
			if !isEval {
				isEval = true
				missingValue = false
				eval.SetZero()
				continue
			}
			isEval = false
			if missingValue {
				toResolve = append(toResolve, unsolvedVariable)
			} else {
				// we have to append our accumulator
				toResolve = append(toResolve, eval.String())
			}
			continue
		}
		cID, vID, visibility := log.ToResolve[j].Unpack()

		if isEval {
			// we are evaluating
			if visibility == schema.Virtual {
				// just add the constant
				eval.Add(&eval, &s.coefficients[cID])
				continue
			}
			if !s.solved[vID] {
				missingValue = true
				continue
			}
			tv := s.computeTerm(log.ToResolve[j])
			eval.Add(&eval, &tv)
			continue
		}

		if visibility == schema.Virtual {
			// it's just a constant
			if cID == compiled.CoeffIdMinusOne {
				toResolve = append(toResolve, "-1")
			} else {
				toResolve = append(toResolve, s.coefficients[cID].String())
			}
			continue
		}
		if !(cID == compiled.CoeffIdMinusOne || cID == compiled.CoeffIdOne) {
			toResolve = append(toResolve, s.coefficients[cID].String())
		}
		if !s.solved[vID] {
			toResolve = append(toResolve, unsolvedVariable)
		} else {
			toResolve = append(toResolve, s.values[vID].String())
		}
	}
	return fmt.Sprintf(log.Format, toResolve...)
}

// UnsatisfiedConstraintError wraps an error with useful metadata on the unsatisfied constraint
type UnsatisfiedConstraintError struct {
	Err       error
	CID       int     // constraint ID
	DebugInfo *string // optional debug info
}

func (r *UnsatisfiedConstraintError) Error() string {
	if r.DebugInfo != nil {
		return fmt.Sprintf("constraint #%d is not satisfied: %s", r.CID, *r.DebugInfo)
	}
	return fmt.Sprintf("constraint #%d is not satisfied: %s", r.CID, r.Err.Error())
}
