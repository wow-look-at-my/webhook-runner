package reloadgate

import "time"

// Recorded gating verdicts: every terminal gating status the gate accepts is written down, whether or not it was applicable at the time.
const (
	// Verdicts older than the ordering window are dead weight: a sha too old to be within fetchDepth of the tip can never pass trySwitch anyway.
	verdictTTL = 7 * 24 * time.Hour
	// Count cap so the state file stays small on a busy repo.
	maxVerdicts = 200
)

// verdictRecord is one sha's last known gating state ("success", "failure",
// or "error" — "pending" is not a verdict and is never recorded).
type verdictRecord struct {
	SHA    string    `json:"sha"`
	State  string    `json:"state"`
	SeenAt time.Time `json:"seen_at"`
}

// recordVerdict upserts a sha's gating state and persists. Last write wins, so
// a CI re-run flipping green->red is respected.
func (g *Gate) recordVerdict(sha, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().UTC()
	for i := range g.verdicts {
		if g.verdicts[i].SHA == sha {
			g.verdicts[i].State, g.verdicts[i].SeenAt = state, now
			g.pruneVerdictsLocked(now)
			g.persistLocked()
			return
		}
	}
	g.verdicts = append(g.verdicts, verdictRecord{SHA: sha, State: state, SeenAt: now})
	g.pruneVerdictsLocked(now)
	g.persistLocked()
}

// pruneVerdictsLocked drops expired records, then the oldest until the count
// cap holds. Caller holds g.mu.
func (g *Gate) pruneVerdictsLocked(now time.Time) {
	kept := g.verdicts[:0]
	for _, v := range g.verdicts {
		if now.Sub(v.SeenAt) < verdictTTL {
			kept = append(kept, v)
		}
	}
	g.verdicts = kept
	for len(g.verdicts) > maxVerdicts {
		oldest := 0
		for i := range g.verdicts {
			if g.verdicts[i].SeenAt.Before(g.verdicts[oldest].SeenAt) {
				oldest = i
			}
		}
		g.verdicts = append(g.verdicts[:oldest], g.verdicts[oldest+1:]...)
	}
}

// verdictFor returns the recorded gating state for a sha, if one is on record
// and unexpired.
func (g *Gate) verdictFor(sha string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now().UTC()
	for _, v := range g.verdicts {
		if v.SHA == sha && now.Sub(v.SeenAt) < verdictTTL {
			return v.State, true
		}
	}
	return "", false
}
