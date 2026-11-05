// Package identity gives cara-agent the Node token it presents to cara-server
// (ADR-0031, CARA-138).
//
// An Agent is enrolled once with a one-time join token, which it trades for a
// Node token and keeps in its identity directory. Later starts use the kept
// token and need no join token. Putting a new join token in place and
// restarting enrols the Agent again, which is how a revoked Node comes back.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	// nodeTokenFile holds the Node token the Agent was given.
	nodeTokenFile = "node-token"
	// joinedWithFile holds the hash of the join token that Node token came
	// from, so a new join token can be told from the one already used.
	joinedWithFile = "joined-with.sha256"
)

// ErrNoCredential means the Agent has no Node token and no join token to get
// one with.
var ErrNoCredential = errors.New("no Node token: set JOIN_TOKEN_FILE to enrol this Node, or AGENT_TOKEN_FILE")

// Joiner trades a join token for a Node token.
type Joiner interface {
	Join(ctx context.Context, joinToken string) (string, error)
}

// Config says where the Agent's credentials are.
type Config struct {
	// TokenFile is a Node token given directly (AGENT_TOKEN_FILE). When set it
	// is used as is, and nothing is kept.
	TokenFile string
	// JoinTokenFile is a one-time join token (JOIN_TOKEN_FILE).
	JoinTokenFile string
	// Dir is where the Node token from a join is kept.
	Dir string
	// IsRefusal reports whether a Join error means the join token was
	// refused, as opposed to a failure worth retrying.
	IsRefusal func(error) bool
	// RetryInterval is the wait between join attempts that failed for a reason
	// other than a refusal.
	RetryInterval time.Duration
}

// Resolve returns the Node token the Agent should use, joining first if it has
// none or was given a new join token. It retries a join that fails for any
// reason other than a refusal until ctx ends.
func Resolve(ctx context.Context, cfg Config, joiner Joiner, logger *zap.Logger) (string, error) {
	if cfg.TokenFile != "" {
		token, err := readSecret(cfg.TokenFile)
		if err != nil {
			return "", fmt.Errorf("node token (AGENT_TOKEN_FILE): %w", err)
		}
		return token, nil
	}

	var joinToken string
	if cfg.JoinTokenFile != "" {
		t, err := readSecret(cfg.JoinTokenFile)
		if err != nil {
			return "", fmt.Errorf("join token (JOIN_TOKEN_FILE): %w", err)
		}
		joinToken = t
	}

	kept, err := readKept(cfg.Dir)
	if err != nil {
		return "", err
	}
	if kept.token != "" && (joinToken == "" || hash(joinToken) == kept.joinedWith) {
		logger.Info("Using the Node token kept from enrolment", zap.String("dir", cfg.Dir))
		return kept.token, nil
	}
	if joinToken == "" {
		return "", ErrNoCredential
	}

	if kept.token != "" {
		logger.Info("A new join token is configured: enrolling this Node again")
	} else {
		logger.Info("Enrolling this Node with its join token")
	}
	token, err := join(ctx, cfg, joiner, joinToken, logger)
	if err != nil {
		if kept.token != "" && cfg.IsRefusal != nil && cfg.IsRefusal(err) {
			logger.Warn("The new join token was refused; keeping the Node token already held", zap.Error(err))
			return kept.token, nil
		}
		return "", err
	}
	if err := keep(cfg.Dir, token, hash(joinToken)); err != nil {
		return "", err
	}
	logger.Info("Node enrolled; its token is kept for later starts", zap.String("dir", cfg.Dir))
	return token, nil
}

func join(ctx context.Context, cfg Config, joiner Joiner, joinToken string, logger *zap.Logger) (string, error) {
	interval := cfg.RetryInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		token, err := joiner.Join(ctx, joinToken)
		if err == nil {
			return token, nil
		}
		if cfg.IsRefusal != nil && cfg.IsRefusal(err) {
			return "", fmt.Errorf("join: %w", err)
		}
		logger.Warn("Joining failed, retrying", zap.Duration("in", interval), zap.Error(err))
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("join: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

type keptIdentity struct {
	token      string
	joinedWith string
}

func readKept(dir string) (keptIdentity, error) {
	token, err := os.ReadFile(filepath.Join(dir, nodeTokenFile))
	if errors.Is(err, os.ErrNotExist) {
		return keptIdentity{}, nil
	}
	if err != nil {
		return keptIdentity{}, fmt.Errorf("read kept Node token: %w", err)
	}
	joinedWith, err := os.ReadFile(filepath.Join(dir, joinedWithFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return keptIdentity{}, fmt.Errorf("read kept join token hash: %w", err)
	}
	return keptIdentity{
		token:      strings.TrimSpace(string(token)),
		joinedWith: strings.TrimSpace(string(joinedWith)),
	}, nil
}

// keep writes the Node token and the join token's hash. Each file is written to
// a temporary name and renamed, so a crash leaves the old file or the new one.
// The token goes first: a crash between the two then re-joins with the same,
// now used, join token, which is refused, and the new token is kept.
func keep(dir, token, joinedWith string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, nodeTokenFile), token+"\n"); err != nil {
		return fmt.Errorf("keep Node token: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, joinedWithFile), joinedWith+"\n"); err != nil {
		return fmt.Errorf("keep join token hash: %w", err)
	}
	return nil
}

func writeAtomic(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readSecret(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
