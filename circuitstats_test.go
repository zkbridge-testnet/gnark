package gnark

import (
	"encoding/gob"
	"os"
	"testing"

	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/backend"
)

type circuitStats struct {
	NbConstraints, Internal, Secret, Public int
}

var mStats map[string][backend.PLONK + 1][ecc.BW6_633 + 1]circuitStats

func checkStats(t *testing.T, circuitName string, nbConstraints, internal, secret, public int, curve ecc.ID, backendID backend.ID) {
	if generateNewStats {
		rs := mStats[circuitName]
		rs[backendID][curve] = circuitStats{nbConstraints, internal, secret, public}
		mStats[circuitName] = rs
		return
	}
	if referenceStats, ok := mStats[circuitName]; !ok {
		t.Log("warning: no stats for circuit", circuitName)
	} else {
		ref := referenceStats[backendID][curve]
		if ref.NbConstraints != nbConstraints {
			t.Errorf("expected %d nbConstraints (reference), got %d. %s, %s, %s", ref.NbConstraints, nbConstraints, circuitName, backendID.String(), curve.String())
		}
		if ref.Internal != internal {
			t.Errorf("expected %d internal (reference), got %d. %s, %s, %s", ref.Internal, internal, circuitName, backendID.String(), curve.String())
		}
		if ref.Secret != secret {
			t.Errorf("expected %d secret (reference), got %d. %s, %s, %s", ref.Secret, secret, circuitName, backendID.String(), curve.String())
		}
		if ref.Public != public {
			t.Errorf("expected %d public (reference), got %d. %s, %s, %s", ref.Public, public, circuitName, backendID.String(), curve.String())
		}
	}
}

func init() {
	mStats = make(map[string][backend.PLONK + 1][ecc.BW6_633 + 1]circuitStats)

	if !generateNewStats {
		fStats, err := os.Open(fileStats)
		if err != nil {
			panic(err)
		}
		decoder := gob.NewDecoder(fStats)
		err = decoder.Decode(&mStats)
		if err != nil {
			panic(err)
		}
	}

}