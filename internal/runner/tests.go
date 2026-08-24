package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// DefaultTestTimeout caps a single test command when the caller doesn't
// specify one. Deliberately independent of the hook's run timeout: that
// one is sized for production work (e.g. long LLM calls), not unit tests.
const DefaultTestTimeout = 10 * time.Minute

// TestOptions configure RunHookTests.
type TestOptions struct {
	Docker string // docker binary; "" = "docker"

	// UsernsRemapped mirrors Options.UsernsRemapped: run/test parity means a
	// hook's declared tests meet the same seccomp.userns interlock a live run
	// meets, so an entity that cannot be served here fails in CI rather than on
	// a runner. false is the safe default.
	UsernsRemapped bool
	Timeout        time.Duration // per-command cap; <= 0 = DefaultTestTimeout
	Out            io.Writer     // combined progress + container output; nil = io.Discard

	// The enforced-GitHub-gateway injection is UNCONDITIONAL (see
	// GSMBaseURL) and applies to test containers too — run/test parity,
	// the dind rule — so there is nothing to configure here: a test that
	// calls api.github.com directly fails loudly instead of depending on
	// production GitHub. Tests are hermetic by contract.
}

// RunHookTests executes the hook's declared test commands (hook.json
// "tests"), each in a fresh container of the hook's built image (built
// first if needed), so tests exercise exactly the bytes a live run
// would. Nothing else from a live run applies: no payload, no hook env,
// no secrets — tests must be self-contained.
//
// All commands run even if an earlier one fails; the returned error
// aggregates every failure (nil when all passed or none are declared).
func RunHookTests(hook *hooks.Hook, opts TestOptions) error {
	if len(hook.Tests) == 0 {
		return nil
	}
	docker := opts.Docker
	if docker == "" {
		docker = "docker"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	image, _, err := EnsureImage(docker, hook, out)
	if err != nil {
		return fmt.Errorf("%s: %w", hook.ID, err)
	}

	var failures []string
	for i, argv := range hook.Tests {
		label := fmt.Sprintf("%s: test %d/%d", hook.ID, i+1, len(hook.Tests))
		fmt.Fprintf(out, "=== %s: %s\n", label, strings.Join(argv, " "))
		start := time.Now()
		if err := runOneTest(docker, hook, image, argv, timeout, out, opts.UsernsRemapped); err != nil {
			fmt.Fprintf(out, "--- %s FAILED after %s: %v\n", label, time.Since(start).Round(time.Millisecond), err)
			failures = append(failures, fmt.Sprintf("test %d (%s): %v", i+1, strings.Join(argv, " "), err))
			continue
		}
		fmt.Fprintf(out, "--- %s passed (%s)\n", label, time.Since(start).Round(time.Millisecond))
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s: %s", hook.ID, strings.Join(failures, "; "))
	}
	return nil
}

// testStorageArgs is the live-run path's storage, backed by tmpfs. That parity
// is what makes read_only_rootfs PROVABLE instead of hopeful: a hook writing
// somewhere it never declared fails its own tests, in CI, before the fleet runs
// it. `scratch` is tmpfs here because a CI runner has no scratch filesystem,
// and what the test establishes is that a writable path IS a mount, not which
// disk backs it.
func testStorageArgs(hook *hooks.Hook) []string {
	var args []string
	for _, p := range append(append([]string{}, hook.Scratch...), hook.Tmpfs...) {
		args = append(args, "--tmpfs", p)
	}
	if hook.ReadOnlyRootfs {
		args = append(args, "--read-only")
	}
	return args
}

func runOneTest(docker string, hook *hooks.Hook, image string, argv []string, timeout time.Duration, out io.Writer, usernsRemapped bool) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("generate container name: %w", err)
	}
	name := "webhook-runner-test-" + hex.EncodeToString(suffix)

	// The same builder the live-run and manager paths use, which is what makes
	// run/test parity a property rather than a habit: a test container gets the
	// mirror routing, the storage flags and the dind mount from the same code,
	// and cannot quietly diverge on the isolation a hook's tests are supposed
	// to cover.
	//
	// What a test deliberately does NOT get is stated by omission: no secrets,
	// no payload/headers, no declared volumes or networks. Tests must be
	// self-contained, so there is nothing to suppress -- the fields stay unset.
	spec := containerSpec{
		name:               name,
		image:              image,
		env:                []string{"HOOK_ID=" + hook.ID},
		storage:            testStorageArgs(hook),
		dind:               hook.Dind,
		dindStorageCovered: hook.ScratchCovers(dindStorageDir),
		argv:               argv,
	}
	// Same run/test parity for seccomp.userns: a hook whose tests exercise a
	// sandbox (bwrap, dats' default backend) needs the relaxed profile here
	// too, or its declared tests could never cover what its live runs do.
	// No Runner here, so no configured tmpDir: "" means the OS default,
	// which is right for a one-shot test container.
	seccompFlags, seccompCleanup, err := seccompArgs(hook, "", hex.EncodeToString(suffix), usernsRemapped)
	if err != nil {
		return err
	}
	defer seccompCleanup()
	spec.seccomp = seccompFlags

	cmd := exec.Command(docker, spec.args()...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start docker: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		// Same rationale as execute(): kill the container by name, not the
		// docker CLI — a SIGKILLed CLI can leave the container running. The
		// fallback Process.Kill only fires if the CLI itself wedges.
		_ = exec.Command(docker, "kill", name).Run()
		killTimer := time.AfterFunc(2*time.Second, func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		defer killTimer.Stop()
		<-done
		return fmt.Errorf("timed out after %s", timeout)
	}
}
