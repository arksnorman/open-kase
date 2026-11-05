package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/package/product"
)

// runKeys implements `keys rotate-jwt`: an offline operator command (it
// needs MONGODB_URI and the master key file) that replaces the active JWT
// signing key. The retired key keeps verifying tokens for the access-token
// lifetime plus the key-cache interval, so signed-in users are unaffected.
func runKeys(ctx context.Context, args []string, stdio IO) int {
	if len(args) == 0 || args[0] != "rotate-jwt" {
		fmt.Fprintf(stdio.Err, "Usage:\n  %s keys rotate-jwt [--overlap DURATION] [--json]\n", product.Binary)
		return ExitUsage
	}
	boot := bootstrapFromEnv()
	fs := newFlagSet("keys rotate-jwt", "Create a new active JWT signing key. The previous key stops signing within a minute on every "+
		"API process and keeps verifying already issued access tokens until they expire. Audited as signingKey.rotated.")
	boot.registerFlags(fs, false)
	overlap := fs.Duration("overlap", 0, "how long the previous key keeps verifying (default and minimum: access-token lifetime + 2m)")
	asJSON := fs.Bool("json", false, "print JSON")
	if code, stop := parseFlags(fs, args[1:], stdio); stop {
		return code
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
	snap, err := c.Settings.Refresh(ctx)
	if err != nil || snap == nil {
		fmt.Fprintf(stdio.Err, "settings: %v (is the installation initialized?)\n", err)
		return ExitFailure
	}
	minOverlap := auth.RotationOverlap(c.authPolicy().AccessTTL)
	if *overlap < minOverlap {
		if *overlap != 0 {
			fmt.Fprintf(stdio.Err, "--overlap raised to %s (access-token lifetime plus key-cache interval)\n", minOverlap)
		}
		*overlap = minOverlap
	}
	rot, err := c.SigningKeys.Rotate(ctx, snap.WorkspaceID(), *overlap, cliOperator("keys rotate-jwt"))
	if err != nil {
		fmt.Fprintf(stdio.Err, "rotate: %v\n", err)
		return ExitFailure
	}
	if *asJSON {
		_ = json.NewEncoder(stdio.Out).Encode(map[string]any{"newKeyId": rot.NewKeyId, "previousKeyId": rot.OldKeyId,
			"previousVerifiesUntil": rot.VerifyUntil})
		return ExitOK
	}
	if rot.OldKeyId == "" {
		fmt.Fprintf(stdio.Out, "created signing key %s (no previous key existed)\n", rot.NewKeyId)
		return ExitOK
	}
	fmt.Fprintf(stdio.Out, "new signing key %s is active; previous key %s verifies existing tokens until %s\n",
		rot.NewKeyId, rot.OldKeyId, rot.VerifyUntil.UTC().Format(time.RFC3339))
	return ExitOK
}
