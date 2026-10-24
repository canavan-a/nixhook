package job

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/canavan-a/nixhook/internal/flake"
	"github.com/canavan-a/nixhook/internal/system"
)

const (
	// bootTimeout bounds how long verify waits for systemd to finish
	// starting units after a switch.
	bootTimeout = 2 * time.Minute
	// rollbackTimeout bounds a rollback, which still runs if the job was
	// cancelled.
	rollbackTimeout = 10 * time.Minute
)

// Runner executes queued jobs. It runs inside the transient deploy unit, not
// the server, so a switch that restarts the server does not interrupt it.
type Runner struct {
	Store    *Store
	Source   flake.Source
	Rollback bool
	// Settle is how long verify waits after the switch before checking for
	// failed units, giving restarted services time to crash.
	Settle time.Duration
}

// Run executes the job with the given ID and records the result. It returns
// the deploy error, if any, after the failure has been saved.
func (r *Runner) Run(ctx context.Context, id string) error {
	j, err := r.Store.Get(id)
	if err != nil {
		return err
	}
	if j.Status != Queued {
		return fmt.Errorf("job %s is %s, not queued", id, j.Status)
	}
	log, err := os.OpenFile(r.Store.LogPath(id), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()

	d := &deploy{Runner: r, job: j, log: log}
	return d.execute(ctx)
}

// deploy is the state of one Run.
type deploy struct {
	*Runner
	job *Job
	log *os.File
	// uri is the flake reference later stages build. For remote sources
	// sync pins it to the resolved commit.
	uri          string
	failedBefore []string
}

func (d *deploy) execute(ctx context.Context) error {
	j := d.job
	now := time.Now().UTC()
	j.Status = Running
	j.StartedAt = &now
	d.save()

	d.logf("nixhook: deploying %s from %s (ref %s)", j.Target, d.Source, j.Ref)
	d.uri = d.Source.URI(j.Ref)
	if gen, err := system.Generation(); err == nil {
		j.GenBefore = gen
	} else {
		d.logf("warning: read system generation: %v", err)
	}
	if d.Source.Local() {
		if rev, err := d.Source.Rev(ctx, j.Ref); err == nil {
			j.RevBefore = rev
		}
	}
	failed, err := system.FailedUnits(ctx)
	if err != nil {
		d.logf("warning: list failed units: %v", err)
	}
	d.failedBefore = failed

	for i := range j.Stages {
		if err = d.stage(ctx, i); err != nil {
			break
		}
	}

	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("cancelled: %w", err)
		}
		j.Fail(err.Error())
		d.save()
		d.logf("\n==> failed: %v", err)
		return err
	}
	end := time.Now().UTC()
	j.Status = Succeeded
	j.FinishedAt = &end
	d.save()
	d.logf("\n==> succeeded in %s", j.Duration().Round(time.Second))
	return nil
}

func (d *deploy) stage(ctx context.Context, i int) error {
	st := &d.job.Stages[i]
	start := time.Now().UTC()
	st.Status = Running
	st.StartedAt = &start
	d.job.Stage = st.Name
	d.save()
	d.logf("\n==> [%s]", st.Name)

	var err error
	switch st.Name {
	case StageSync:
		err = d.sync(ctx)
	case StageBuild:
		err = d.build(ctx)
	case StageSwitch:
		err = d.switchSystem(ctx)
	case StageVerify:
		err = d.verify(ctx)
	default:
		err = fmt.Errorf("unknown stage %q", st.Name)
	}

	end := time.Now().UTC()
	st.FinishedAt = &end
	if err != nil {
		st.Status = Failed
		st.Error = err.Error()
	} else {
		st.Status = Succeeded
	}
	d.save()
	return err
}

// sync brings the source up to date: git pull for a checkout, or resolving
// the ref to a commit for a remote flake.
func (d *deploy) sync(ctx context.Context) error {
	j := d.job
	if d.Source.Local() {
		if err := d.run(ctx, "git", d.Source.GitArgs("pull", "--ff-only", "origin", j.Ref)...); err != nil {
			return err
		}
		rev, err := d.Source.Rev(ctx, j.Ref)
		if err != nil {
			return err
		}
		j.RevAfter = rev
		d.logf("at %s", rev)
		return nil
	}
	rev, err := d.Source.Rev(ctx, j.Ref)
	if err != nil {
		return err
	}
	j.RevAfter = rev
	d.uri = d.Source.Pinned(rev)
	d.logf("resolved %s to %s", d.Source.URI(j.Ref), rev)
	return nil
}

func (d *deploy) build(ctx context.Context) error {
	j := d.job
	targets, err := d.Source.Targets(ctx, d.uri)
	if err != nil {
		return err
	}
	if !slices.Contains(targets, j.Target) {
		return fmt.Errorf("flake has no nixosConfigurations.%s (available: %s)", j.Target, strings.Join(targets, ", "))
	}
	out, err := d.output(ctx, "nix", "build", "--no-link", "--print-out-paths", "-L", flake.Toplevel(d.uri, j.Target))
	if err != nil {
		return err
	}
	j.OutPath = out
	d.logf("built %s", out)
	return nil
}

func (d *deploy) switchSystem(ctx context.Context) error {
	j := d.job
	err := d.run(ctx, "nixos-rebuild", "switch", "--flake", flake.Config(d.uri, j.Target))
	if gen, genErr := system.Generation(); genErr == nil {
		j.GenAfter = gen
	}
	if err != nil {
		d.rollback(ctx)
		return err
	}
	d.logf("now at generation %d", j.GenAfter)
	return nil
}

// verify waits for systemd to settle and fails if the switch left units
// failed that were not failed before it.
func (d *deploy) verify(ctx context.Context) error {
	deadline := time.Now().Add(bootTimeout)
	state := system.State(ctx)
	for (state == "initializing" || state == "starting") && time.Now().Before(deadline) {
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
		state = system.State(ctx)
	}
	d.logf("waiting %s for services to settle", d.Settle)
	if err := sleep(ctx, d.Settle); err != nil {
		return err
	}

	failed, err := system.FailedUnits(ctx)
	if err != nil {
		return err
	}
	var newly []string
	for _, u := range failed {
		if !slices.Contains(d.failedBefore, u) {
			newly = append(newly, u)
		}
	}
	d.logf("system %s, %d failed units (%d new)", system.State(ctx), len(failed), len(newly))
	if len(newly) > 0 {
		d.rollback(ctx)
		return fmt.Errorf("units failed after switch: %s", strings.Join(newly, ", "))
	}
	return nil
}

// rollback switches back to the previous generation when enabled and the
// deploy actually created a new one.
func (d *deploy) rollback(ctx context.Context) {
	j := d.job
	if !d.Rollback || j.GenAfter == 0 || j.GenAfter == j.GenBefore {
		return
	}
	d.logf("\n==> rolling back to generation %d", j.GenBefore)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := d.run(ctx, "nixos-rebuild", "switch", "--rollback"); err != nil {
		d.logf("rollback failed: %v", err)
		return
	}
	j.RolledBack = true
	if gen, err := system.Generation(); err == nil {
		j.GenAfter = gen
	}
}

// run executes a command with its output appended to the job log.
func (d *deploy) run(ctx context.Context, name string, args ...string) error {
	return d.exec(ctx, d.log, name, args...)
}

// output is run that also returns the command's trimmed stdout.
func (d *deploy) output(ctx context.Context, name string, args ...string) (string, error) {
	var stdout strings.Builder
	err := d.exec(ctx, io.MultiWriter(d.log, &stdout), name, args...)
	return strings.TrimSpace(stdout.String()), err
}

func (d *deploy) exec(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	d.logf("$ %s %s", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Stdout = stdout
	cmd.Stderr = d.log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (d *deploy) logf(format string, args ...any) {
	fmt.Fprintf(d.log, format+"\n", args...)
}

// save persists the job. A failed write is logged rather than aborting the
// deploy, since the switch itself matters more than its bookkeeping.
func (d *deploy) save() {
	if err := d.Store.Save(d.job); err != nil {
		d.logf("warning: save job state: %v", err)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
