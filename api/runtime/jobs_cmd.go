package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/user"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/product"
)

func runWorker(ctx context.Context, args []string, stdio IO) int {
	boot := bootstrapFromEnv()
	fs := newFlagSet("worker", "Run the outbox dispatcher and background job workers without the HTTP server. "+
		"Use it with `serve --workers=false`, or alongside serve: jobs are leased in MongoDB, so several workers never run the same attempt.")
	boot.registerFlags(fs, false)
	concurrency := fs.Int("concurrency", 4, "number of concurrent job workers (1-64)")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	if *concurrency < 1 || *concurrency > 64 {
		fmt.Fprintln(stdio.Err, "configuration error:\n--concurrency must be between 1 and 64")
		return ExitUsage
	}
	c, code := setup(boot, false, stdio)
	if c == nil {
		return code
	}
	defer c.Close(context.Background())
	if code := ensureSchema(ctx, c, stdio); code != ExitOK {
		return code
	}
	var wg sync.WaitGroup
	wg.Go(func() { c.watchSettings(ctx) })
	wg.Go(func() { c.runWorkers(ctx, "worker", *concurrency) })
	wg.Wait()
	return ExitOK
}

func cliOperator(command string) audit.Actor {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return audit.Operator(audit.SystemCLI, "cli "+command+" (os user "+name+")")
}

func runJobs(ctx context.Context, args []string, stdio IO) int {
	if len(args) == 0 || (args[0] != "list" && args[0] != "retry") {
		fmt.Fprintf(stdio.Err, "Usage:\n  %s jobs list [--state queued|running|succeeded|dead] [--kind KIND] [--limit N] [--json]\n  %s jobs retry <job id>\n",
			product.Binary, product.Binary)
		return ExitUsage
	}
	sub, rest := args[0], args[1:]
	boot := bootstrapFromEnv()
	fs := newFlagSet("jobs "+sub, "Inspect or retry durable background jobs.")
	boot.registerFlags(fs, false)
	state := fs.String("state", "", "filter by state: queued, running, succeeded or dead")
	kind := fs.String("kind", "", "filter by kind, e.g. email.notification, webhook.delivery, assignments.release")
	limit := fs.Int("limit", 50, "maximum number of jobs (1-500)")
	asJSON := fs.Bool("json", false, "print JSON")
	var id string
	if sub == "retry" {
		if len(rest) == 0 || rest[0] == "" || rest[0][0] == '-' {
			fmt.Fprintf(stdio.Err, "Usage: %s jobs retry <job id>\n", product.Binary)
			return ExitUsage
		}
		id, rest = rest[0], rest[1:]
	}
	if code, stop := parseFlags(fs, rest, stdio); stop {
		return code
	}
	switch jobs.State(*state) {
	case "", jobs.StateQueued, jobs.StateRunning, jobs.StateSucceeded, jobs.StateDead:
	default:
		fmt.Fprintln(stdio.Err, "--state must be queued, running, succeeded or dead")
		return ExitUsage
	}
	c, code := setup(boot, false, stdio)
	if c == nil {
		return code
	}
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if code := ensureSchema(ctx, c, stdio); code != ExitOK {
		return code
	}

	if sub == "list" {
		rows, err := c.Jobs.List(ctx, jobs.Filter{State: jobs.State(*state), Kind: *kind, Limit: *limit})
		if err != nil {
			fmt.Fprintf(stdio.Err, "jobs: %v\n", err)
			return ExitFailure
		}
		if *asJSON {
			_ = json.NewEncoder(stdio.Out).Encode(rows)
			return ExitOK
		}
		tw := tabwriter.NewWriter(stdio.Out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tKIND\tSTATE\tATTEMPTS\tRUN AT\tLAST ERROR")
		for _, j := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", j.Id, j.Kind, j.State, j.Attempts, j.MaxAttempts,
				j.RunAt.Format(time.RFC3339), j.LastClass)
		}
		_ = tw.Flush()
		return ExitOK
	}

	// retry: a job with a delivery record is requeued together with it.
	_, derr := c.NotifyRepo.GetDelivery(ctx, id)
	var err error
	switch {
	case derr == nil:
		_, err = c.Notifications.Requeue(ctx, cliOperator("jobs retry"), "", id)
	case errors.Is(derr, notifications.ErrNotFound):
		_, err = c.Jobs.Requeue(ctx, id, c.Clock.Now(), nil)
		if err == nil {
			c.Waker.Notify(ctx)
		}
	default:
		err = derr
	}
	switch {
	case errors.Is(err, jobs.ErrNotFound), fault.As(err).Code == "deliveryNotFound":
		fmt.Fprintf(stdio.Err, "job %s not found\n", id)
		return ExitFailure
	case errors.Is(err, jobs.ErrNotRetryable), fault.As(err).Code == "deliveryNotRetryable":
		fmt.Fprintf(stdio.Err, "job %s is not dead-lettered (only dead jobs and failed or unknown-outcome deliveries can be retried)\n", id)
		return ExitFailure
	case err != nil:
		fmt.Fprintf(stdio.Err, "retry: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdio.Out, "job %s queued for another attempt\n", id)
	return ExitOK
}
