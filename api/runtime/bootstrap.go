// Package runtime is the composition root: bootstrap configuration,
// dependency wiring, process lifecycle and command dispatch. Import it as
// appruntime to avoid confusion with the standard library runtime package.
package runtime

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/arksnorman/open-kase/api/package/logger"
	"github.com/arksnorman/open-kase/api/package/product"
)

// Environments.
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
	EnvTest        = "test"
)

// Bootstrap is the small set of values needed before MongoDB-held settings
// can load. Product settings never come from the environment.
type Bootstrap struct {
	Env            string // APP_ENV
	MongoURI       string // MONGODB_URI
	RedisURI       string // REDIS_URI
	MasterKeyFile  string // OPEN_KASE_MASTER_KEY_FILE / --master-key-file
	HTTPAddr       string // OPEN_KASE_HTTP_ADDR / --http-addr
	LogLevel       string // OPEN_KASE_LOG_LEVEL / --log-level
	LogFormat      string // OPEN_KASE_LOG_FORMAT / --log-format
	TrustedProxies string // OPEN_KASE_TRUSTED_PROXIES / --trusted-proxies (comma separated)
	UIDir          string // OPEN_KASE_UI_DIR / --ui-dir: serve the built SPA (optional)
}

// DefaultMasterKeyFile is used when no path is configured.
const DefaultMasterKeyFile = "/etc/" + product.Slug + "/master.key"

func envOr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// bootstrapFromEnv reads the environment layer.
func bootstrapFromEnv() Bootstrap {
	p := product.EnvPrefix
	return Bootstrap{
		Env:            envOr("APP_ENV", EnvProduction),
		MongoURI:       envOr("MONGODB_URI", ""),
		RedisURI:       envOr("REDIS_URI", ""),
		MasterKeyFile:  envOr(p+"MASTER_KEY_FILE", DefaultMasterKeyFile),
		HTTPAddr:       envOr(p+"HTTP_ADDR", ":8080"),
		LogLevel:       envOr(p+"LOG_LEVEL", "info"),
		LogFormat:      envOr(p+"LOG_FORMAT", ""),
		TrustedProxies: envOr(p+"TRUSTED_PROXIES", ""),
		UIDir:          envOr(p+"UI_DIR", ""),
	}
}

// registerFlags binds bootstrap flags to fs with environment defaults; flags
// take precedence over the environment.
func (b *Bootstrap) registerFlags(fs *flag.FlagSet, withHTTP bool) {
	fs.StringVar(&b.MasterKeyFile, "master-key-file", b.MasterKeyFile, "path to the master key file (env "+product.EnvPrefix+"MASTER_KEY_FILE)")
	fs.StringVar(&b.LogLevel, "log-level", b.LogLevel, "log level: debug, info, warn, error")
	fs.StringVar(&b.LogFormat, "log-format", b.LogFormat, "log format: json or text (default json in production, text otherwise)")
	if withHTTP {
		fs.StringVar(&b.HTTPAddr, "http-addr", b.HTTPAddr, "HTTP listen address, e.g. 0.0.0.0:8080 (env "+product.EnvPrefix+"HTTP_ADDR)")
		fs.StringVar(&b.TrustedProxies, "trusted-proxies", b.TrustedProxies, "comma-separated proxy IPs/CIDRs whose X-Forwarded-For is trusted")
		fs.StringVar(&b.UIDir, "ui-dir", b.UIDir, "serve the built SPA (e.g. ui/dist) from this directory (env "+product.EnvPrefix+"UI_DIR)")
	}
}

// validate checks the bootstrap values that every command needs.
func (b Bootstrap) validate(needRedis bool) error {
	var errs []error
	switch b.Env {
	case EnvProduction, EnvDevelopment, EnvTest:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be production, development or test (got %q)", b.Env))
	}
	if b.MongoURI == "" {
		errs = append(errs, errors.New("MONGODB_URI is required"))
	}
	if needRedis && b.RedisURI == "" {
		errs = append(errs, errors.New("REDIS_URI is required"))
	}
	if b.MasterKeyFile == "" {
		errs = append(errs, errors.New("a master key file path is required"))
	}
	if b.LogFormat != "" && b.LogFormat != string(logger.FormatJSON) && b.LogFormat != string(logger.FormatText) {
		errs = append(errs, fmt.Errorf("log format must be json or text (got %q)", b.LogFormat))
	}
	if _, err := logger.ParseLevel(b.LogLevel); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b Bootstrap) trustedProxies() []string {
	var out []string
	for _, p := range strings.Split(b.TrustedProxies, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newLogger builds the single process logger.
func (b Bootstrap) newLogger(w io.Writer) *slog.Logger {
	level, _ := logger.ParseLevel(b.LogLevel)
	format := logger.Format(b.LogFormat)
	if format == "" {
		format = logger.FormatText
		if b.Env == EnvProduction {
			format = logger.FormatJSON
		}
	}
	return logger.New(w, logger.Options{Format: format, Level: level}).With("service", product.Slug)
}
