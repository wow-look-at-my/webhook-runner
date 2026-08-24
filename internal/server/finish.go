package server

import (
	"fmt"
	"log/slog"

	"github.com/wow-look-at-my/webhook-runner/internal/events"
	"github.com/wow-look-at-my/webhook-runner/internal/kv"
	"github.com/wow-look-at-my/webhook-runner/internal/runs"
)

// RunFinishCallback builds the run tracker's OnFinish observer — the exactly-once-per-run finish seam (it fires on EVERY terminal path: success, error, timeout kill, cancel; wired in cli/serve.go). It lives here, next to the state API's lock handlers, because it is the other half of the lock contract: acquire/release bind locks to the calling run, and THIS is what guarantees a run's locks never outlive it. It does two things, in a deliberate order: 1.
func RunFinishCallback(kvStore *kv.Store, record func(runs.RunState) error, rec *events.Recorder, logger *slog.Logger) func(runs.RunState) {
	return func(st runs.RunState) {
		if n := kvStore.ReleaseRunLocks(st.ID); n > 0 {
			logger.Info("released leftover run locks", "hook", st.HookID, "run", st.ID, "count", n)
			rec.Record("lock.released_on_finish",
				fmt.Sprintf("%s: run %s finished still holding %d lock(s) — released", st.HookID, st.ID, n),
				map[string]string{"hook": st.HookID})
		}
		if err := record(st); err != nil {
			logger.Error("persist finished run", "hook", st.HookID, "run", st.ID, "err", err)
		}
	}
}
