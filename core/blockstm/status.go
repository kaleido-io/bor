package blockstm

import (
	"fmt"
	"sort"
)

func makeStatusManager(numTasks int) (t taskStatusManager) {
	t.pending = make([]int, numTasks)
	for i := 0; i < numTasks; i++ {
		t.pending[i] = i
	}

	t.dependency = make(map[int]map[int]bool, numTasks)
	t.blockCount = make(map[int]map[int]bool, numTasks)

	return
}

type taskStatusManager struct {
	pending    []int
	inProgress []int
	complete   []int
	dependency map[int]map[int]bool
	blockCount map[int]map[int]bool
}

func insertInList(l []int, v int) []int {
	if len(l) == 0 || v > l[len(l)-1] {
		return append(l, v)
	} else {
		x := sort.SearchInts(l, v)
		if x < len(l) && l[x] == v {
			// already in list
			return l
		}
		a := append(l[:x+1], l[x:]...)
		a[x] = v
		return a
	}
}

func (m *taskStatusManager) takeNextPending() int {
	if len(m.pending) == 0 {
		return -1
	}

	x := m.pending[0]
	m.pending = m.pending[1:]
	m.inProgress = insertInList(m.inProgress, x)

	return x
}

func hasNoGap(l []int) bool {
	return l[0]+len(l) == l[len(l)-1]+1
}

func (m taskStatusManager) maxAllComplete() int {
	if len(m.complete) == 0 || m.complete[0] != 0 {
		return -1
	} else if m.complete[len(m.complete)-1] == len(m.complete)-1 {
		return m.complete[len(m.complete)-1]
	} else {
		for i := len(m.complete) - 2; i >= 0; i-- {
			if hasNoGap(m.complete[:i+1]) {
				return m.complete[i]
			}
		}
	}

	return -1
}

func (m *taskStatusManager) pushPending(tx int) {
	m.pending = insertInList(m.pending, tx)
}

func removeFromList(l []int, v int, expect bool) []int {
	x := sort.SearchInts(l, v)
	if x == -1 || l[x] != v {
		if expect {
			panic(fmt.Errorf("should not happen - element expected in list"))
		}

		return l
	}

	switch x {
	case 0:
		return l[1:]
	case len(l) - 1:
		return l[:len(l)-1]
	default:
		return append(l[:x], l[x+1:]...)
	}
}

func (m *taskStatusManager) markComplete(tx int) {
	m.inProgress = removeFromList(m.inProgress, tx, true)
	m.complete = insertInList(m.complete, tx)
}

func (m *taskStatusManager) minPending() int {
	if len(m.pending) == 0 {
		return -1
	} else {
		return m.pending[0]
	}
}

func (m *taskStatusManager) countComplete() int {
	return len(m.complete)
}

func (m *taskStatusManager) addDependencies(txs []int, dependent int) bool {
	for _, tx := range txs {
		x := sort.SearchInts(m.complete, tx)
		if x < len(m.complete) && m.complete[x] == tx {
			// Blocking tx has already completed
			delete(m.blockCount[dependent], tx)
			continue
		}

		if _, ok := m.dependency[tx]; !ok {
			m.dependency[tx] = make(map[int]bool)
		}

		if _, ok := m.blockCount[dependent]; !ok {
			m.blockCount[dependent] = make(map[int]bool)
		}

		m.dependency[tx][dependent] = true
		m.blockCount[dependent][tx] = true
	}

	return len(m.blockCount[dependent]) > 0
}

func (m *taskStatusManager) removeDependency(tx int) {
	if deps, ok := m.dependency[tx]; ok && len(deps) > 0 {
		for k := range deps {
			delete(m.blockCount[k], tx)

			if len(m.blockCount[k]) == 0 {
				m.pushPending(k)
			}
		}

		delete(m.dependency, tx)
	}
}

func (m *taskStatusManager) clearInProgress(tx int) {
	m.inProgress = removeFromList(m.inProgress, tx, true)
}

func (m *taskStatusManager) checkInProgress(tx int) bool {
	x := sort.SearchInts(m.inProgress, tx)
	if x < len(m.inProgress) && m.inProgress[x] == tx {
		return true
	}

	return false
}

func (m *taskStatusManager) checkPending(tx int) bool {
	x := sort.SearchInts(m.pending, tx)
	if x < len(m.pending) && m.pending[x] == tx {
		return true
	}

	return false
}

// getRevalidationRange: this range will be all tasks from tx (inclusive) that are not currently in progress up to the
//  'all complete' limit
func (m *taskStatusManager) getRevalidationRange(txFrom int) (ret []int) {
	max := m.maxAllComplete() // haven't learned to trust compilers :)
	for x := txFrom; x <= max; x++ {
		if !m.checkInProgress(x) {
			ret = append(ret, x)
		}
	}

	return
}

func (m *taskStatusManager) pushPendingSet(set []int) {
	for _, v := range set {
		m.pushPending(v)
	}
}

func (m *taskStatusManager) clearComplete(tx int) {
	m.complete = removeFromList(m.complete, tx, false)
}
