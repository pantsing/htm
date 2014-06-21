package htm

import (
	//"fmt"
	//"github.com/cznic/mathutil"
	"github.com/zacg/floats"
	"github.com/zacg/go.matrix"
	//"math"
	//"math/rand"
	//"sort"
)

type TpOutputType int

const (
	Normal                 TpOutputType = 0
	ActiveState            TpOutputType = 1
	ActiveState1CellPerCol TpOutputType = 2
)

type ProcessAction int

const (
	Update ProcessAction = 0
	Keep   ProcessAction = 1
	Remove ProcessAction = 2
)

type TemporalPoolerParams struct {
	NumberOfCols           int
	CellsPerColumn         int
	InitialPerm            float64
	ConnectedPerm          float64
	MinThreshold           int
	NewSynapseCount        int
	PermanenceInc          float64
	PermanenceDec          float64
	PermanenceMax          float64
	GlobalDecay            int
	ActivationThreshold    int
	DoPooling              bool
	SegUpdateValidDuration int
	BurnIn                 int
	CollectStats           bool
	//Seed                   int
	//verbosity=VERBOSITY,
	//checkSynapseConsistency=False, # for cpp only -- ignored
	TrivialPredictionMethods string
	PamLength                int
	MaxInfBacktrack          int
	MaxLrnBacktrack          int
	MaxAge                   int
	MaxSeqLength             int
	MaxSegmentsPerCell       int
	MaxSynapsesPerSegment    int
	outputType               TpOutputType
}

type DynamicState struct {
	//orginally dynamic vars
	lrnActiveState     *SparseBinaryMatrix // t
	lrnActiveStateLast *SparseBinaryMatrix // t-1

	lrnPredictedState     *SparseBinaryMatrix
	lrnPredictedStateLast *SparseBinaryMatrix

	infActiveState          *SparseBinaryMatrix
	infActiveStateLast      *SparseBinaryMatrix
	infActiveStateBackup    *SparseBinaryMatrix
	infActiveStateCandidate *SparseBinaryMatrix

	infPredictedState          *SparseBinaryMatrix
	infPredictedStateLast      *SparseBinaryMatrix
	infPredictedStateBackup    *SparseBinaryMatrix
	infPredictedStateCandidate *SparseBinaryMatrix

	cellConfidence          *matrix.DenseMatrix
	cellConfidenceLast      *matrix.DenseMatrix
	cellConfidenceCandidate *matrix.DenseMatrix

	colConfidence          []float64
	colConfidenceLast      []float64
	colConfidenceCandidate []float64
}

func (ds *DynamicState) Copy() *DynamicState {
	result := new(DynamicState)
	result.lrnActiveState = ds.lrnActiveState.Copy()
	result.lrnActiveStateLast = ds.lrnActiveStateLast.Copy()

	result.lrnPredictedState = ds.lrnPredictedState.Copy()
	result.lrnPredictedStateLast = ds.lrnPredictedStateLast.Copy()

	result.infActiveState = ds.infActiveState.Copy()
	result.infActiveStateLast = ds.infActiveStateLast.Copy()
	result.infActiveStateBackup = ds.infActiveStateBackup.Copy()
	result.infActiveStateCandidate = ds.infActiveStateCandidate.Copy()

	result.infPredictedState = ds.infPredictedState.Copy()
	result.infPredictedStateLast = ds.infPredictedStateLast.Copy()
	result.infPredictedStateBackup = ds.infPredictedStateBackup.Copy()
	result.infPredictedStateCandidate = ds.infPredictedStateCandidate.Copy()

	result.cellConfidence = ds.cellConfidence.Copy()
	result.cellConfidenceCandidate = ds.cellConfidenceCandidate.Copy()
	result.cellConfidenceLast = ds.cellConfidenceLast.Copy()

	copy(result.colConfidence, ds.colConfidence)
	copy(result.colConfidenceCandidate, ds.colConfidenceCandidate)
	copy(result.colConfidenceLast, ds.colConfidenceLast)

	return result
}

type TemporalPooler struct {
	params              TemporalPoolerParams
	numberOfCells       int
	activeColumns       []int
	cells               [][][]Segment
	lrnIterationIdx     int
	iterationIdx        int
	segId               int
	CurrentOutput       *SparseBinaryMatrix
	pamCounter          int
	avgInputDensity     float64
	avgLearnedSeqLength float64
	resetCalled         bool

	//ephemeral state
	segmentUpdates map[TupleInt][]UpdateState
	/*
	 	 NOTE: We don't use the same backtrack buffer for inference and learning
	     because learning has a different metric for determining if an input from
	     the past is potentially useful again for backtracking.

	     Our inference backtrack buffer. This keeps track of up to
	     maxInfBacktrack of previous input. Each entry is a list of active column
	     inputs.
	*/
	prevInfPatterns [][]int

	/*
			 Our learning backtrack buffer. This keeps track of up to maxLrnBacktrack
		     of previous input. Each entry is a list of active column inputs
	*/

	prevLrnPatterns [][]int

	DynamicState *DynamicState
}

func NewTemportalPooler(tParams TemporalPoolerParams) *TemporalPooler {
	tp := new(TemporalPooler)

	//validate args
	if tParams.PamLength <= 0 {
		panic("Pam length must be > 0")
	}

	//Fixed size CLA mode
	if tParams.MaxSegmentsPerCell != -1 || tParams.MaxSynapsesPerSegment != -1 {
		//validate args
		if tParams.MaxSegmentsPerCell <= 0 {
			panic("Maxsegs must be greater than 0")
		}
		if tParams.MaxSynapsesPerSegment <= 0 {
			panic("Max syns per segment must be greater than 0")
		}
		if tParams.GlobalDecay != 0.0 {
			panic("Global decay must be 0")
		}
		if tParams.MaxAge != 0 {
			panic("Max age must be 0")
		}
		if !(tParams.MaxSynapsesPerSegment >= tParams.NewSynapseCount) {
			panic("maxSynapsesPerSegment must be >= newSynapseCount")
		}

		tp.numberOfCells = tParams.NumberOfCols * tParams.CellsPerColumn

		// No point having larger expiration if we are not doing pooling
		if !tParams.DoPooling {
			tParams.SegUpdateValidDuration = 1
		}

		//Cells are indexed by column and index in the column
		// Every self.cells[column][index] contains a list of segments
		// Each segment is a structure of class Segment

		//TODO: initialize cells

		tp.lrnIterationIdx = 0
		tp.iterationIdx = 0
		tp.segId = 0

		// pamCounter gets reset to pamLength whenever we detect that the learning
		// state is making good predictions (at least half the columns predicted).
		// Whenever we do not make a good prediction, we decrement pamCounter.
		// When pamCounter reaches 0, we start the learn state over again at start
		// cells.
		tp.pamCounter = tParams.PamLength

	}

	return tp
}

//Returns new segId
func (su *TemporalPooler) GetSegId() int {
	result := su.segId
	su.segId++
	return result
}

/*
	 Compute the column confidences given the cell confidences. If
	None is passed in for cellConfidences, it uses the stored cell confidences
	from the last compute.

	param cellConfidences Cell confidences to use, or None to use the
	the current cell confidences.

	returns Column confidence scores
*/

func (su *TemporalPooler) columnConfidences() []float64 {
	//ignore cellconfidence param for now
	return su.DynamicState.colConfidence
}

/*
 Top-down compute - generate expected input given output of the TP
	param topDownIn top down input from the level above us
	returns best estimate of the TP input that would have generated bottomUpOut.
*/

func (su *TemporalPooler) topDownCompute() []float64 {
	/*
			 For now, we will assume there is no one above us and that bottomUpOut is
		     simply the output that corresponds to our currently stored column
		     confidences.

		     Simply return the column confidences
	*/

	return su.columnConfidences()
}

/*
 This function gives the future predictions for <nSteps> timesteps starting
from the current TP state. The TP is returned to its original state at the
end before returning.

- We save the TP state.
- Loop for nSteps
- Turn-on with lateral support from the current active cells
- Set the predicted cells as the next step's active cells. This step
in learn and infer methods use input here to correct the predictions.
We don't use any input here.
- Revert back the TP state to the time before prediction

param nSteps The number of future time steps to be predicted
returns all the future predictions - a numpy array of type "float32" and
shape (nSteps, numberOfCols).
The ith row gives the tp prediction for each column at
a future timestep (t+i+1).
*/

func (tp *TemporalPooler) predict(nSteps int) *matrix.DenseMatrix {
	// Save the TP dynamic state, we will use to revert back in the end
	pristineTPDynamicState := tp.DynamicState.Copy()

	if nSteps <= 0 {
		panic("nSteps must be greater than zero")
	}

	// multiStepColumnPredictions holds all the future prediction.
	var elements []float64
	multiStepColumnPredictions := matrix.MakeDenseMatrix(elements, nSteps, tp.params.NumberOfCols)

	// This is a (nSteps-1)+half loop. Phase 2 in both learn and infer methods
	// already predicts for timestep (t+1). We use that prediction for free and
	// save the half-a-loop of work.

	step := 0
	for {
		multiStepColumnPredictions.FillRow(step, tp.topDownCompute())
		if step == nSteps-1 {
			break
		}
		step += 1

		//Copy t-1 into t
		tp.DynamicState.infActiveState = tp.DynamicState.infActiveStateLast
		tp.DynamicState.infPredictedState = tp.DynamicState.infPredictedStateLast
		tp.DynamicState.cellConfidence = tp.DynamicState.cellConfidenceLast

		// Predicted state at "t-1" becomes the active state at "t"
		tp.DynamicState.infActiveState = tp.DynamicState.infPredictedState

		// Predicted state and confidence are set in phase2.
		tp.DynamicState.infPredictedState.Clear()
		tp.DynamicState.cellConfidence.Fill(0.0)
		tp.inferPhase2()
	}

	// Revert the dynamic state to the saved state
	tp.DynamicState = pristineTPDynamicState

	return multiStepColumnPredictions

}

/*
 This routine computes the activity level of a segment given activeState.
It can tally up only connected synapses (permanence >= connectedPerm), or
all the synapses of the segment, at either t or t-1.
*/

func (tp *TemporalPooler) getSegmentActivityLevel(seg Segment, activeState *SparseBinaryMatrix, connectedSynapsesOnly bool) int {
	activity := 0
	if connectedSynapsesOnly {
		for _, val := range seg.syns {
			if val.Permanence >= tp.params.ConnectedPerm {
				if activeState.Get(val.SrcCellIdx, val.SrcCellCol) {
					activity++
				}
			}
		}
	} else {
		for _, val := range seg.syns {
			if activeState.Get(val.SrcCellIdx, val.SrcCellCol) {
				activity++
			}
		}
	}

	return activity
}

/*
	 A segment is active if it has >= activationThreshold connected
	synapses that are active due to activeState.
*/

func (tp *TemporalPooler) isSegmentActive(seg Segment, activeState *SparseBinaryMatrix) bool {

	if len(seg.syns) < tp.params.ActivationThreshold {
		return false
	}

	activity := 0
	for _, val := range seg.syns {
		if val.Permanence >= tp.params.ConnectedPerm {
			if activeState.Get(val.SrcCellIdx, val.SrcCellCol) {
				activity++
				if activity >= tp.params.ActivationThreshold {
					return true
				}
			}

		}
	}

	return false
}

/*
 Phase 2 for the inference state. The computes the predicted state, then
checks to insure that the predicted state is not over-saturated, i.e.
look too close like a burst. This indicates that there were so many
separate paths learned from the current input columns to the predicted
input columns that bursting on the current input columns is most likely
generated mix and match errors on cells in the predicted columns. If
we detect this situation, we instead turn on only the start cells in the
current active columns and re-generate the predicted state from those.

returns True if we have a decent guess as to the next input.
Returing False from here indicates to the caller that we have
reached the end of a learned sequence.

This looks at:
- infActiveState

This modifies:
-  infPredictedState
-  colConfidence
-  cellConfidence
*/

func (tp *TemporalPooler) inferPhase2() bool {
	// Init to zeros to start
	tp.DynamicState.infPredictedState.Clear()
	tp.DynamicState.cellConfidence.Fill(0)
	FillSliceFloat64(tp.DynamicState.colConfidence, 0)

	// Phase 2 - Compute new predicted state and update cell and column
	// confidences
	for c := 0; c < tp.params.NumberOfCols; c++ {
		for i := 0; i < tp.params.CellsPerColumn; i++ {
			// For each segment in the cell
			for _, seg := range tp.cells[c][i] {
				// Check if it has the min number of active synapses
				numActiveSyns := tp.getSegmentActivityLevel(seg, tp.DynamicState.infActiveState, false)
				if numActiveSyns < tp.params.ActivationThreshold {
					continue
				}

				//Incorporate the confidence into the owner cell and column
				dc := seg.dutyCycle(false, false)
				tp.DynamicState.cellConfidence.Set(c, i, tp.DynamicState.cellConfidence.Get(c, i)+dc)
				tp.DynamicState.colConfidence[c] += dc

				if tp.isSegmentActive(seg, tp.DynamicState.infActiveState) {
					tp.DynamicState.infPredictedState.Set(c, i, true)
				}
			}
		}

	}

	// Normalize column and cell confidences
	sumConfidences := SumSliceFloat64(tp.DynamicState.colConfidence)

	if sumConfidences > 0 {
		floats.DivConst(sumConfidences, tp.DynamicState.colConfidence)
		tp.DynamicState.cellConfidence.DivScaler(sumConfidences)
	}

	// Are we predicting the required minimum number of columns?
	numPredictedCols := float64(tp.DynamicState.infPredictedState.TotalTrueCols())

	return numPredictedCols >= (0.5 * tp.avgInputDensity)

}

/*
Computes output for both learning and inference. In both cases, the
output is the boolean OR of activeState and predictedState at t.
Stores currentOutput for checkPrediction.
*/

func (tp *TemporalPooler) computeOutput() []bool {

	switch tp.params.outputType {
	case ActiveState1CellPerCol:
		// Fire only the most confident cell in columns that have 2 or more
		// active cells

		mostActiveCellPerCol := tp.DynamicState.cellConfidence.ArgMaxCols()
		tp.CurrentOutput = NewSparseBinaryMatrix(tp.DynamicState.infActiveState.Height, tp.DynamicState.infActiveState.Width)

		// Turn on the most confident cell in each column. Note here that
		// Columns refers to TP columns, even though each TP column is a row
		// in the matrix.
		for i := 0; i < tp.CurrentOutput.Height; i++ {
			//only on active cols
			if len(tp.DynamicState.infActiveState.GetRowIndices(i)) != 0 {
				tp.CurrentOutput.Set(i, mostActiveCellPerCol[i], true)
			}
		}

		break
	case ActiveState:
		tp.CurrentOutput = tp.DynamicState.infActiveState.Copy()
		break
	case Normal:
		tp.CurrentOutput = tp.DynamicState.infPredictedState.Or(tp.DynamicState.infActiveState)
		break
	default:
		panic("Unknown output type")
	}

	return tp.CurrentOutput.Flatten()
}

/*
Update our moving average of learned sequence length.
*/

func (tp *TemporalPooler) updateAvgLearnedSeqLength(prevSeqLength float64) {
	alpha := 0.0
	if tp.lrnIterationIdx < 100 {
		alpha = 0.5
	} else {
		alpha = 0.1
	}

	tp.avgLearnedSeqLength = ((1.0-alpha)*tp.avgLearnedSeqLength + (alpha * prevSeqLength))
}

/*
 Update the inference active state from the last set of predictions
and the current bottom-up.

This looks at:
- infPredictedState['t-1']
This modifies:
- infActiveState['t']

param activeColumns list of active bottom-ups
param useStartCells If true, ignore previous predictions and simply turn on
the start cells in the active columns
returns True if the current input was sufficiently predicted, OR
if we started over on startCells.
False indicates that the current input was NOT predicted,
and we are now bursting on most columns.
*/

func (tp *TemporalPooler) inferPhase1(activeColumns []int, useStartCells bool) bool {
	// Start with empty active state
	tp.DynamicState.infActiveState.Clear()

	// Phase 1 - turn on predicted cells in each column receiving bottom-up
	// If we are following a reset, activate only the start cell in each
	// column that has bottom-up
	numPredictedColumns := 0
	if useStartCells {
		for _, val := range activeColumns {
			tp.DynamicState.infActiveState.Set(val, 0, true)
		}
	} else {
		// else, turn on any predicted cells in each column. If there are none, then
		// turn on all cells (burst the column)
		for _, val := range activeColumns {
			predictingCells := tp.DynamicState.infPredictedStateLast.GetRowIndices(val)
			numPredictingCells := len(predictingCells)

			if numPredictingCells > 0 {
				//may have to set instead of replace
				tp.DynamicState.infActiveState.ReplaceRowByIndices(val, predictingCells)
				numPredictedColumns++
			} else {
				tp.DynamicState.infActiveState.FillRow(val, true) // whole column bursts
			}
		}
	}

	// Did we predict this input well enough?
	return useStartCells || numPredictedColumns >= int(0.50*float64(len(activeColumns)))

}

/*
 This "backtracks" our inference state, trying to see if we can lock onto
the current set of inputs by assuming the sequence started up to N steps
ago on start cells.

@param activeColumns The list of active column indices

This will adjust @ref infActiveState['t'] if it does manage to lock on to a
sequence that started earlier. It will also compute infPredictedState['t']
based on the possibly updated @ref infActiveState['t'], so there is no need to
call inferPhase2() after calling inferBacktrack().

This looks at:
- @ref infActiveState['t']

This updates/modifies:
- @ref infActiveState['t']
- @ref infPredictedState['t']
- @ref colConfidence['t']
- @ref cellConfidence['t']

How it works:
-------------------------------------------------------------------
This method gets called from updateInferenceState when we detect either of
the following two conditions:
-# The current bottom-up input had too many un-expected columns
-# We fail to generate a sufficient number of predicted columns for the
next time step.

Either of these two conditions indicate that we have fallen out of a
learned sequence.

Rather than simply "giving up" and bursting on the unexpected input
columns, a better approach is to see if perhaps we are in a sequence that
started a few steps ago. The real world analogy is that you are driving
along and suddenly hit a dead-end, you will typically go back a few turns
ago and pick up again from a familiar intersection.

This back-tracking goes hand in hand with our learning methodology, which
always tries to learn again from start cells after it loses context. This
results in a network that has learned multiple, overlapping paths through
the input data, each starting at different points. The lower the global
decay and the more repeatability in the data, the longer each of these
paths will end up being.

The goal of this function is to find out which starting point in the past
leads to the current input with the most context as possible. This gives us
the best chance of predicting accurately going forward. Consider the
following example, where you have learned the following sub-sequences which
have the given frequencies:

? - Q - C - D - E 10X seq 0
? - B - C - D - F 1X seq 1
? - B - C - H - I 2X seq 2
? - B - C - D - F 3X seq 3
? - Z - A - B - C - D - J 2X seq 4
? - Z - A - B - C - H - I 1X seq 5
? - Y - A - B - C - D - F 3X seq 6

----------------------------------------
W - X - Z - A - B - C - D <= input history
^
current time step

Suppose, in the current time step, the input pattern is D and you have not
predicted D, so you need to backtrack. Suppose we can backtrack up to 6
steps in the past, which path should we choose? From the table above, we can
see that the correct answer is to assume we are in seq 1. How do we
implement the backtrack to give us this right answer? The current
implementation takes the following approach:

-# Start from the farthest point in the past.
-# For each starting point S, calculate the confidence of the current
input, conf(startingPoint=S), assuming we followed that sequence.
Note that we must have learned at least one sequence that starts at
point S.
-# If conf(startingPoint=S) is significantly different from
conf(startingPoint=S-1), then choose S-1 as the starting point.

The assumption here is that starting point S-1 is the starting point of
a learned sub-sequence that includes the current input in it's path and
that started the longest ago. It thus has the most context and will be
the best predictor going forward.

From the statistics in the above table, we can compute what the confidences
will be for each possible starting point:

startingPoint confidence of D
-----------------------------------------
B (t-2) 4/6 = 0.667 (seq 1,3)/(seq 1,2,3)
Z (t-4) 2/3 = 0.667 (seq 4)/(seq 4,5)

First of all, we do not compute any confidences at starting points t-1, t-3,
t-5, t-6 because there are no learned sequences that start at those points.

Notice here that Z is the starting point of the longest sub-sequence leading
up to the current input. Event though starting at t-2 and starting at t-4
give the same confidence value, we choose the sequence starting at t-4
because it gives the most context, and it mirrors the way that learning
extends sequences.
*/

func (tp *TemporalPooler) inferBacktrack(activeColumns []int) {
	// How much input history have we accumulated?
	// The current input is always at the end of self._prevInfPatterns (at
	// index -1), but it is also evaluated as a potential starting point by
	// turning on it's start cells and seeing if it generates sufficient
	// predictions going forward.
	numPrevPatterns := len(tp.prevInfPatterns)
	if numPrevPatterns <= 0 {
		return
	}

	// This is an easy to use label for the current time step
	currentTimeStepsOffset := numPrevPatterns - 1

	// Save our current active state in case we fail to find a place to restart
	// todo: save infActiveState['t-1'], infPredictedState['t-1']?
	tp.DynamicState.infActiveStateBackup = tp.DynamicState.infActiveStateLast.Copy()

	// Save our t-1 predicted state because we will write over it as as evaluate
	// each potential starting point.
	tp.DynamicState.infPredictedStateBackup = tp.DynamicState.infPredictedStateLast

	// We will record which previous input patterns did not generate predictions
	// up to the current time step and remove all the ones at the head of the
	// input history queue so that we don't waste time evaluating them again at
	// a later time step.
	var badPatterns []int

	// Let's go back in time and replay the recent inputs from start cells and
	// see if we can lock onto this current set of inputs that way.

	// Start the farthest back and work our way forward. For each starting point,
	// See if firing on start cells at that point would predict the current
	// input as well as generate sufficient predictions for the next time step.

	// We want to pick the point closest to the current time step that gives us
	// the relevant confidence. Think of this example, where we are at D and need
	// to
	// A - B - C - D
	// decide if we should backtrack to C, B, or A. Suppose B-C-D is a high order
	// sequence and A is unrelated to it. If we backtrock to B would we get a
	// certain confidence of D, but if went went farther back, to A, the
	// confidence wouldn't change, since A has no impact on the B-C-D series.

	// So, our strategy will be to pick the "B" point, since choosing the A point
	// does not impact our confidences going forward at all.
	inSequence := false
	candConfidence := -1.0
	candStartOffset := 0

	//for startOffset in range(0, numPrevPatterns):
	for startOffset := 0; startOffset < numPrevPatterns; startOffset++ {
		// If we have a candidate already in the past, don't bother falling back
		// to start cells on the current input.
		if startOffset == currentTimeStepsOffset && candConfidence != -1 {
			break
		}

		// Play through starting from starting point 'startOffset'
		inSequence = false
		totalConfidence := 0.0
		//for offset in range(startOffset, numPrevPatterns):
		for offset := startOffset; offset < numPrevPatterns; offset++ {
			// If we are about to set the active columns for the current time step
			// based on what we predicted, capture and save the total confidence of
			// predicting the current input

			if offset == currentTimeStepsOffset {
				for _, val := range activeColumns {
					totalConfidence += tp.DynamicState.colConfidence[val]
				}
			}

			// Compute activeState[t] given bottom-up and predictedState @ t-1
			tp.DynamicState.infPredictedStateLast = tp.DynamicState.infPredictedState

			inSequence = tp.inferPhase1(tp.prevInfPatterns[offset], (offset == startOffset))
			if !inSequence {
				break
			}
			// Compute predictedState at t given activeState at t
			inSequence = tp.inferPhase2()
			if !inSequence {
				break
			}

		}

		// If starting from startOffset got lost along the way, mark it as an
		// invalid start point.
		if !inSequence {
			badPatterns = append(badPatterns, startOffset)
			continue
		}

		// If we got to here, startOffset is a candidate starting point.
		// Save this state as a candidate state. It will become the chosen state if
		// we detect a change in confidences starting at a later startOffset
		candConfidence = totalConfidence
		candStartOffset = startOffset

		if candStartOffset == currentTimeStepsOffset { // no more to try
			break
		}
		tp.DynamicState.infActiveStateCandidate = tp.DynamicState.infActiveState.Copy()
		tp.DynamicState.infPredictedStateCandidate = tp.DynamicState.infPredictedState.Copy()
		tp.DynamicState.cellConfidenceCandidate = tp.DynamicState.cellConfidence.Copy()
		copy(tp.DynamicState.colConfidenceCandidate, tp.DynamicState.colConfidence)
		break

	}

	// If we failed to lock on at any starting point, fall back to the original
	// active state that we had on entry
	if candStartOffset == -1 {
		tp.DynamicState.infActiveState = tp.DynamicState.infActiveStateBackup
		tp.inferPhase2()
	} else {
		// Install the candidate state, if it wasn't the last one we evaluated.
		if candStartOffset != currentTimeStepsOffset {
			tp.DynamicState.infActiveState = tp.DynamicState.infActiveStateCandidate
			tp.DynamicState.infPredictedState = tp.DynamicState.infPredictedStateCandidate
			tp.DynamicState.cellConfidence = tp.DynamicState.cellConfidenceCandidate
			tp.DynamicState.colConfidence = tp.DynamicState.colConfidenceCandidate
		}

	}

	// Remove any useless patterns at the head of the previous input pattern
	// queue.
	for i := 0; i < numPrevPatterns; i++ {
		if ContainsInt(i, badPatterns) || (candStartOffset != -1 && i <= candStartOffset) {
			//pop prev pattern
			tp.prevInfPatterns = tp.prevInfPatterns[:len(tp.prevInfPatterns)-1]
		} else {
			break
		}
	}

	// Restore the original predicted state.
	tp.DynamicState.infPredictedState = tp.DynamicState.infPredictedStateBackup
}

/*
 Update the inference state. Called from compute() on every iteration.
param activeColumns The list of active column indices.
*/

func (tp *TemporalPooler) updateInferenceState(activeColumns []int) {

	// Copy t to t-1
	tp.DynamicState.infActiveStateLast = tp.DynamicState.infActiveState.Copy()
	tp.DynamicState.infPredictedStateLast = tp.DynamicState.infPredictedState.Copy()
	tp.DynamicState.cellConfidenceLast = tp.DynamicState.cellConfidence.Copy()
	copy(tp.DynamicState.colConfidenceLast, tp.DynamicState.colConfidence)

	// Each phase will zero/initilize the 't' states that it affects

	// Update our inference input history
	if tp.params.MaxInfBacktrack > 0 {
		if len(tp.prevInfPatterns) > tp.params.MaxInfBacktrack {
			//pop prev pattern
			tp.prevInfPatterns = tp.prevInfPatterns[:len(tp.prevInfPatterns)-1]
		}
		tp.prevInfPatterns = append(tp.prevInfPatterns, activeColumns)
	}

	// Compute the active state given the predictions from last time step and
	// the current bottom-up
	inSequence := tp.inferPhase1(activeColumns, tp.resetCalled)

	// If this input was considered unpredicted, let's go back in time and
	// replay the recent inputs from start cells and see if we can lock onto
	// this current set of inputs that way.
	if !inSequence {
		// inferBacktrack() will call inferPhase2() for us.
		tp.inferBacktrack(activeColumns)
		return
	}

	// Compute the predicted cells and the cell and column confidences
	inSequence = tp.inferPhase2()

	if !inSequence {
		// inferBacktrack() will call inferPhase2() for us.
		tp.inferBacktrack(activeColumns)
	}

}

/*
Remove a segment update (called when seg update expires or is processed)
*/

func (tp *TemporalPooler) removeSegmentUpdate(updateState UpdateState) {
	// Key is stored in segUpdate itself...
	key := TupleInt{updateState.Update.columnIdx, updateState.Update.cellIdx}
	delete(tp.segmentUpdates, key)
}

/*
 Removes any update that would be for the given col, cellIdx, segIdx.
NOTE: logically, we need to do this when we delete segments, so that if
an update refers to a segment that was just deleted, we also remove
that update from the update list. However, I haven't seen it trigger
in any of the unit tests yet, so it might mean that it's not needed
and that situation doesn't occur, by construction.
*/

func (tp *TemporalPooler) cleanUpdatesList(col, cellIdx int, seg Segment) {
	for idx, val := range tp.segmentUpdates {
		if idx.A == col && idx.B == cellIdx {
			for _, update := range val {
				if update.Update.segment == seg {
					tp.removeSegmentUpdate(update)
				}
			}
		}
	}

}