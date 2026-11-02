// Package sim is a deterministic discrete-event simulator for the merge
// queue. It runs the real queue.Engine against a synthetic repository,
// a fake forge and a fake CI whose outcomes come from a ground-truth model
// (which PRs are broken, which pairs break each other, which tests are
// flaky), and checks the core invariant on every merge.
package sim

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"time"

	"github.com/xartus2004/greenline/queue"
)

// simPR is the ground truth for one PR.
type simPR struct {
	id       queue.PRID
	version  int
	bad      map[int]bool // per version: independently broken
	partners []queue.PRID // PRs it breaks when combined with
	touched  []int        // directly modified targets
	affected []int        // reverse-dependency closure (sorted)
	arrival  time.Time
}

// commit is a node in the fake repo's history.
type commit struct {
	id       string
	parent   *commit
	items    []queue.Item
	bad      bool     // ground truth: tree is broken
	culprits []string // failing tests that truth implies
	state    []uint64 // per-target content hash (cache keys)
	pairs    map[queue.PRID]bool
	external bool
}

// World is the fake forge plus ground truth.
type World struct {
	p       Params
	rng     *rand.Rand
	prs     map[queue.PRID]*simPR
	commits map[string]*commit
	main    *commit
	history []*commit // main, oldest first

	// targets: deps[i] are targets i depends on (all < i).
	deps  [][]int
	rdeps [][]int

	// CI-reported outcomes per commit, for the invariant checker.
	passedCI  map[string]bool
	flakyOnly map[string]bool // CI failed only on known-flaky tests
	partnerOf map[queue.PRID]bool
	// flakyTests are tests that fail randomly with FlakeRate.
	flakyTests []string

	Violations []string
}

func newWorld(p Params, rng *rand.Rand) *World {
	w := &World{p: p, rng: rng, prs: map[queue.PRID]*simPR{}, commits: map[string]*commit{}, passedCI: map[string]bool{}, flakyOnly: map[string]bool{}, partnerOf: map[queue.PRID]bool{}}
	// Layered synthetic DAG: each target depends on up to 2 lower ones.
	w.deps = make([][]int, p.Targets)
	w.rdeps = make([][]int, p.Targets)
	for i := 1; i < p.Targets; i++ {
		n := rng.Intn(3)
		seen := map[int]bool{}
		for k := 0; k < n; k++ {
			d := rng.Intn(i)
			if !seen[d] {
				seen[d] = true
				w.deps[i] = append(w.deps[i], d)
				w.rdeps[d] = append(w.rdeps[d], i)
			}
		}
	}
	for i := 0; i < p.FlakyTests; i++ {
		w.flakyTests = append(w.flakyTests, fmt.Sprintf("//t%d:flaky_test", rng.Intn(p.Targets)))
	}
	root := &commit{id: "root", state: make([]uint64, p.Targets), pairs: map[queue.PRID]bool{}}
	w.commits[root.id] = root
	w.main = root
	w.history = []*commit{root}
	return w
}

func (w *World) closure(touched []int) []int {
	seen := map[int]bool{}
	var walk func(int)
	walk = func(t int) {
		if seen[t] {
			return
		}
		seen[t] = true
		for _, r := range w.rdeps[t] {
			walk(r)
		}
	}
	for _, t := range touched {
		walk(t)
	}
	out := make([]int, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Ints(out)
	return out
}

func targetName(t int) string { return fmt.Sprintf("//t%d:t", t) }

func labels(ts []int) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = targetName(t)
	}
	sort.Strings(out)
	return out
}

func h64(parts ...any) uint64 {
	f := fnv.New64a()
	fmt.Fprint(f, parts...)
	return f.Sum64()
}

// apply builds (or finds) the commit parent+group. It is deterministic, as
// queue.CI requires: the same parent and items always give the same id.
func (w *World) apply(parent *commit, group []queue.Item) *commit {
	id := fmt.Sprintf("%016x", h64(parent.id, group))
	if c, ok := w.commits[id]; ok {
		return c
	}
	c := &commit{id: id, parent: parent, items: group, bad: parent.bad,
		state: append([]uint64(nil), parent.state...), pairs: parent.pairs}
	c.culprits = append(c.culprits, parent.culprits...)
	copied := false
	for _, it := range group {
		pr := w.prs[it.PR]
		for _, t := range pr.affected {
			c.state[t] = h64(c.state[t], it.PR, it.Version)
		}
		if pr.bad[it.Version] {
			c.bad = true
			c.culprits = append(c.culprits, fmt.Sprintf("%s#pr%d", targetName(pr.touched[0]), it.PR))
		}
		for _, q := range pr.partners {
			if c.pairs[q] {
				c.bad = true
				c.culprits = append(c.culprits, fmt.Sprintf("conflict(pr%d,pr%d)", q, it.PR))
			}
		}
		// Record membership for PRs that take part in conflict pairs.
		if len(pr.partners) > 0 || w.isPartner(it.PR) {
			if !copied {
				m := make(map[queue.PRID]bool, len(c.pairs)+1)
				for k, v := range c.pairs {
					m[k] = v
				}
				c.pairs, copied = m, true
			}
			c.pairs[it.PR] = true
		}
	}
	w.commits[id] = c
	return c
}

func (w *World) isPartner(id queue.PRID) bool { return w.partnerOf[id] }

// --- queue.Forge

// Head implements queue.Forge.
func (w *World) Head() (string, error) { return w.main.id, nil }

// Advance implements queue.Forge and checks the invariant on every merge.
func (w *World) Advance(old, next string) error {
	if old != w.main.id {
		return queue.ErrStale
	}
	c, ok := w.commits[next]
	if !ok {
		return fmt.Errorf("unknown commit %s", next)
	}
	if c.parent != w.main {
		return fmt.Errorf("refusing non-fast-forward: %s's parent is not main", next)
	}
	if !w.passedCI[next] && !w.flakyOnly[next] {
		w.Violations = append(w.Violations, fmt.Sprintf("merged %s without a passing CI run on that exact commit", next))
	}
	if c.bad {
		w.Violations = append(w.Violations, fmt.Sprintf("main broken by %s: %v", next, c.culprits))
	}
	w.main = c
	w.history = append(w.history, c)
	return nil
}

// Contains implements queue.Forge.
func (w *World) Contains(id string) (bool, error) {
	for _, c := range w.history {
		if c.id == id {
			return true, nil
		}
	}
	return false, nil
}

// externalPush lands a (green) commit on main bypassing the queue.
func (w *World) externalPush(n int) {
	c := &commit{id: fmt.Sprintf("ext%d-%016x", n, h64(w.main.id)), parent: w.main, bad: w.main.bad,
		state: append([]uint64(nil), w.main.state...), pairs: w.main.pairs, external: true}
	// Touch a random target so cache keys downstream change.
	t := w.rng.Intn(w.p.Targets)
	for _, a := range w.closure([]int{t}) {
		c.state[a] = h64(c.state[a], c.id)
	}
	w.commits[c.id] = c
	w.main = c
	w.history = append(w.history, c)
}

// mainContains reports whether PR id is in main's tree, and at which
// version.
func (w *World) mainItems() map[queue.PRID]int {
	out := map[queue.PRID]int{}
	for _, c := range w.history {
		for _, it := range c.items {
			if _, dup := out[it.PR]; dup {
				w.Violations = append(w.Violations, fmt.Sprintf("PR %d landed twice", it.PR))
			}
			out[it.PR] = it.Version
		}
	}
	return out
}
