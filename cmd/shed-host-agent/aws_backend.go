package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AWSBackend performs AWS credential operations on the host.
type AWSBackend interface {
	// GetCredentials returns temporary AWS credentials for the given shed on
	// the given server.
	GetCredentials(ctx context.Context, server, shedName string) (*AWSCachedCredentials, error)

	// Status returns the role and cache expiration for the given shed on the
	// given server.
	Status(server, shedName string) (role string, cachedUntil *time.Time)
}

// AWSCachedCredentials holds a cached set of STS temporary credentials. For
// passthrough mode, Expiration may be the zero value when the source profile
// carries no expiry hint (the guest then discovers expiry on a 403).
type AWSCachedCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// stsBackend serves AWS credentials, either by AssumeRole (assume-role mode) or
// by vending the source profile's existing session credentials (passthrough
// mode). The STS client is built lazily so a passthrough-only config starts even
// when no AssumeRole credentials are resolvable from an SSO/SAML source profile.
type stsBackend struct {
	cfg           AWSConfig
	refreshBefore time.Duration
	sessionDur    time.Duration
	logger        *slog.Logger

	mu    sync.Mutex
	cache map[string]*AWSCachedCredentials

	clientOnce sync.Once
	client     *sts.Client
	clientErr  error
}

// NewSTSBackend creates an AWS backend. It validates durations and reports
// whether AWS is configured at all; the STS client itself is created on first
// AssumeRole use (see stsClient).
func NewSTSBackend(cfg AWSConfig, logger *slog.Logger) (AWSBackend, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("no AWS credentials configured (set aws.default_role, aws.mode: passthrough, or a per-server/shed role/mode)")
	}

	refreshBefore, err := time.ParseDuration(cfg.CacheRefreshBefore)
	if err != nil {
		logger.Warn("invalid cache_refresh_before, using default", "value", cfg.CacheRefreshBefore, "default", "5m")
		refreshBefore = 5 * time.Minute
	}

	sessionDur, err := time.ParseDuration(cfg.SessionDuration)
	if err != nil {
		logger.Warn("invalid session_duration, using default", "value", cfg.SessionDuration, "default", "1h")
		sessionDur = 1 * time.Hour
	}

	logger.Info("AWS backend initialized",
		"profile", cfg.SourceProfile,
		"default_role", cfg.DefaultRole,
		"default_mode", normalizeAWSMode(cfg.Mode),
		"session_duration", sessionDur,
		"cache_refresh_before", refreshBefore,
	)

	return &stsBackend{
		cfg:           cfg,
		refreshBefore: refreshBefore,
		sessionDur:    sessionDur,
		logger:        logger,
		cache:         make(map[string]*AWSCachedCredentials),
	}, nil
}

// stsClient builds (once) and returns the STS client for the source profile.
// Only the assume-role path calls it, so a passthrough-only agent never loads
// the AssumeRole credential chain.
func (b *stsBackend) stsClient(ctx context.Context) (*sts.Client, error) {
	b.clientOnce.Do(func() {
		awsCfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(b.cfg.SourceProfile))
		if err != nil {
			b.clientErr = fmt.Errorf("loading AWS config for profile %q: %w", b.cfg.SourceProfile, err)
			return
		}
		b.client = sts.NewFromConfig(awsCfg)
	})
	return b.client, b.clientErr
}

func (b *stsBackend) GetCredentials(ctx context.Context, server, shedName string) (*AWSCachedCredentials, error) {
	resolved := b.cfg.Resolve(server, shedName)

	if resolved.Mode == AWSModePassthrough {
		return b.getPassthroughCreds(ctx, server, shedName)
	}

	roleARN := resolved.Role
	if roleARN == "" {
		return nil, fmt.Errorf("no role configured for shed %q on server %q", shedName, server)
	}

	// Cache and session names are keyed by server/shed so identical shed names
	// on different servers don't collide.
	cacheKey := serverShedKey(server, shedName)

	// Check cache under lock
	b.mu.Lock()
	if cached, ok := b.cache[cacheKey]; ok {
		if time.Until(cached.Expiration) > b.refreshBefore {
			b.mu.Unlock()
			b.logger.Debug("returning cached credentials", "server", server, "shed", shedName, "expires", cached.Expiration)
			return cached, nil
		}
	}
	b.mu.Unlock()

	client, err := b.stsClient(ctx)
	if err != nil {
		return nil, err
	}

	durationSec := int32(b.resolveSessionDur(resolved).Seconds())

	// Assume role without holding the lock (avoids blocking other sheds)
	sessionName := fmt.Sprintf("shed-%s-%s-%d", server, shedName, time.Now().Unix())

	result, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn:         &roleARN,
		RoleSessionName: &sessionName,
		DurationSeconds: &durationSec,
	})
	if err != nil {
		return nil, fmt.Errorf("sts:AssumeRole failed for %s: %w", roleARN, err)
	}
	if result.Credentials == nil {
		return nil, fmt.Errorf("sts:AssumeRole returned nil credentials for %s", roleARN)
	}

	creds := &AWSCachedCredentials{
		AccessKeyID:     *result.Credentials.AccessKeyId,
		SecretAccessKey: *result.Credentials.SecretAccessKey,
		SessionToken:    *result.Credentials.SessionToken,
		Expiration:      *result.Credentials.Expiration,
	}

	b.mu.Lock()
	b.cache[cacheKey] = creds
	b.mu.Unlock()

	b.logger.Info("assumed role",
		"server", server,
		"shed", shedName,
		"role", roleARN,
		"session", sessionName,
		"expires", creds.Expiration,
	)

	return creds, nil
}

// getPassthroughCreds vends the source profile's existing session credentials
// directly, re-reading the shared credentials file on every request (no cache,
// no lock) so a fresh SSO/SAML login is picked up immediately. It deliberately
// does not touch b.cache/b.mu.
func (b *stsBackend) getPassthroughCreds(ctx context.Context, server, shedName string) (*AWSCachedCredentials, error) {
	profile := b.cfg.SourceProfile
	credsPath := sharedCredentialsPath()
	cfgPath := sharedConfigPath()

	// LoadSharedConfigProfile (unlike LoadDefaultConfig) ignores the
	// AWS_SHARED_CREDENTIALS_FILE / AWS_CONFIG_FILE env vars, so we resolve the
	// paths ourselves and pass them explicitly. It re-reads the files on every
	// call (no provider cache).
	sc, err := config.LoadSharedConfigProfile(ctx, profile, func(o *config.LoadSharedConfigOptions) {
		o.CredentialsFiles = []string{credsPath}
		o.ConfigFiles = []string{cfgPath}
	})
	if err != nil {
		return nil, fmt.Errorf("passthrough: loading profile %q from %s: %w", profile, credsPath, err)
	}

	c := sc.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, fmt.Errorf("passthrough: profile %q in %s has no static credentials; run your SSO/SAML login (e.g. `aws sso login`) to refresh", profile, credsPath)
	}
	if c.SessionToken == "" {
		return nil, fmt.Errorf("passthrough: profile %q has no aws_session_token; passthrough expects temporary SSO/SAML session credentials, not long-lived keys", profile)
	}

	creds := &AWSCachedCredentials{
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		SessionToken:    c.SessionToken,
		Expiration:      parseSessionExpiry(credsPath, profile),
	}

	b.logger.Info("vending passthrough credentials",
		"server", server,
		"shed", shedName,
		"profile", profile,
		"expires", expiryLabel(creds.Expiration),
	)

	return creds, nil
}

// resolveSessionDur returns the session duration for a resolved policy, falling
// back to the backend default when the override is unset or invalid.
func (b *stsBackend) resolveSessionDur(resolved ResolvedAWS) time.Duration {
	if resolved.SessionDuration == "" {
		return b.sessionDur
	}
	return parseDurationOr(resolved.SessionDuration, b.sessionDur, "session_duration", b.logger)
}

func (b *stsBackend) Status(server, shedName string) (string, *time.Time) {
	resolved := b.cfg.Resolve(server, shedName)

	if resolved.Mode == AWSModePassthrough {
		role := "passthrough:" + b.cfg.SourceProfile
		// Scan the file directly (no token validation) so a missing/unreadable
		// file degrades to a nil expiry rather than an error Status can't return.
		if exp := parseSessionExpiry(sharedCredentialsPath(), b.cfg.SourceProfile); !exp.IsZero() {
			return role, &exp
		}
		return role, nil
	}

	role := resolved.Role

	b.mu.Lock()
	defer b.mu.Unlock()

	if cached, ok := b.cache[serverShedKey(server, shedName)]; ok {
		return role, &cached.Expiration
	}
	return role, nil
}

// sharedCredentialsPath resolves the shared credentials file, honoring
// AWS_SHARED_CREDENTIALS_FILE (which LoadSharedConfigProfile does not read).
func sharedCredentialsPath() string {
	if p := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); p != "" {
		return p
	}
	return config.DefaultSharedCredentialsFilename()
}

// sharedConfigPath resolves the shared config file, honoring AWS_CONFIG_FILE.
func sharedConfigPath() string {
	if p := os.Getenv("AWS_CONFIG_FILE"); p != "" {
		return p
	}
	return config.DefaultSharedConfigFilename()
}

// parseSessionExpiry scans the credentials file's [profile] section for a
// session-expiry hint (aws_session_expiration or x_security_token_expires) that
// SSO/SAML helpers conventionally write. The AWS SDK does not surface these keys
// (it treats shared-file creds as non-expiring), so we read them ourselves.
// Returns the zero time when the file, profile, or key is missing or
// unparseable — the caller then omits Expiration so the guest discovers expiry
// on a 403. Only the credentials file (bare [name]) is scanned; hints written
// into ~/.aws/config under [profile <name>] are out of scope.
func parseSessionExpiry(credsPath, profile string) time.Time {
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return time.Time{}
	}
	inSection := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			name = strings.TrimPrefix(name, "profile ") // tolerate config-style headers
			inSection = name == profile
			continue
		}
		if !inSection {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "aws_session_expiration", "x_security_token_expires":
			return parseExpiryValue(strings.TrimSpace(val))
		}
	}
	return time.Time{}
}

// parseExpiryValue parses an expiry hint value defensively across the RFC3339
// variants SSO/SAML helpers emit. Returns the zero time on any failure.
func parseExpiryValue(val string) time.Time {
	val = strings.Trim(val, `"'`)
	for _, layout := range []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z0700",
		"2006-01-02 15:04:05Z07:00",
	} {
		if t, err := time.Parse(layout, val); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// expiryLabel renders an expiration for logs, rendering the zero time as "none"
// rather than the year-0001 default.
func expiryLabel(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.Format(time.RFC3339)
}
