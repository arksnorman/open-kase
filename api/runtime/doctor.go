package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
	"github.com/arksnorman/open-kase/api/package/secrets"
)

func runDoctor(ctx context.Context, args []string, stdio IO) int {
	boot := bootstrapFromEnv()
	fs := newFlagSet("doctor", "Check bootstrap configuration, master key, MongoDB, schema, installation, settings and Redis.")
	boot.registerFlags(fs, false)
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	failed := false
	report := func(level, name, msg string, args ...any) {
		if level == "FAIL" {
			failed = true
		}
		fmt.Fprintf(stdio.Out, "%-4s %-12s %s\n", level, name, fmt.Sprintf(msg, args...))
	}

	if err := boot.validate(true); err != nil {
		report("FAIL", "bootstrap", "%v", err)
		return ExitFailure
	}
	report("OK", "bootstrap", "APP_ENV=%s", boot.Env)

	keys, err := secrets.LoadKeyFile(boot.MasterKeyFile)
	if err != nil {
		report("FAIL", "masterKey", "%v", err)
		return ExitFailure
	}
	report("OK", "masterKey", "loaded %s (key id %s)", boot.MasterKeyFile, keys.ActiveKeyID())

	c, err := newContainer(boot.newLogger(stdio.Err), boot, keys)
	if err != nil {
		report("FAIL", "mongodb", "%v", err)
		return ExitFailure
	}
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	rs, err := c.Store.ReplicaSet(ctx)
	if err != nil {
		report("FAIL", "mongodb", "%v", err)
	} else {
		report("OK", "mongodb", "database %q, replica set %q, %d member(s), server %s", c.Store.Database(), rs.Name, rs.Members, rs.ServerVersion)
		if rs.Members == 1 {
			report("INFO", "mongodb", "single-node replica set: transactions work, but there is no redundancy (not highly available)")
		}
		inst, err := c.Installation.State(ctx)
		switch {
		case err != nil:
			report("FAIL", "installation", "%v", err)
		case !inst.Initialized:
			report("FAIL", "installation", "not initialized; run `%s init`", product.Binary)
		default:
			report("OK", "installation", "workspace %q initialized; master key matches", inst.WorkspaceName)
			if snap, err := c.Settings.Refresh(ctx); err != nil {
				report("FAIL", "settings", "%v", err)
			} else {
				report("OK", "settings", "configVersion %d is valid", snap.Version())
				if snap.Product().BaseURL == "" {
					report("WARN", "settings", "product base URL is not configured; email links cannot be generated yet")
				}
			}
		}
	}

	if err := c.checkRedis(ctx); err != nil {
		report("WARN", "redis", "unreachable (%v); the API runs degraded without cache/presence", err)
	} else {
		report("OK", "redis", "reachable")
	}
	if failed {
		return ExitFailure
	}
	return ExitOK
}
