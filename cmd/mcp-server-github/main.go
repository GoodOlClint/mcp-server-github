// Command mcp-server-github serves the push_verified MCP tool over stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/GoodOlClint/mcp-server-github/internal/github"
	"github.com/GoodOlClint/mcp-server-github/internal/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "v0.1.0"

type repoRoots []string

func (r *repoRoots) String() string { return strings.Join(*r, ",") }

func (r *repoRoots) Set(v string) error {
	if v == "" {
		return errors.New("repo root must not be empty")
	}
	*r = append(*r, v)
	return nil
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("mcp-server-github: ")
	// stdout carries the JSON-RPC stream; diagnostics must not join it.
	log.SetOutput(os.Stderr)

	var roots repoRoots
	fs := flag.NewFlagSet("mcp-server-github", flag.ExitOnError)
	fs.Var(&roots, "repo-root", "directory a repo_path may live inside; repeatable, required")
	endpoint := fs.String("endpoint", "", "GitHub GraphQL endpoint (default https://api.github.com/graphql)")
	timeout := fs.Duration("timeout", 120*time.Second, "per-request timeout for GitHub GraphQL calls")
	maxCommitBytes := fs.Int64("max-commit-bytes", tool.DefaultMaxCommitBytes,
		"per-commit payload ceiling; the measurement behind the default is in ADR 0002 and depends on the uplink")
	// flag.ExitOnError already exits on a parse failure.
	_ = fs.Parse(os.Args[1:])
	if len(roots) == 0 {
		fs.Usage()
		log.Fatal("at least one --repo-root is required")
	}
	if *timeout <= 0 {
		log.Fatal("--timeout must be positive")
	}
	if *maxCommitBytes <= 0 {
		log.Fatal("--max-commit-bytes must be positive")
	}

	if err := run(roots, *endpoint, *timeout, *maxCommitBytes); err != nil {
		log.Fatal(err)
	}
}

func run(roots []string, endpoint string, timeout time.Duration, maxCommitBytes int64) error {
	appID, installationID, keyPath, err := github.FromEnv()
	if err != nil {
		return err
	}
	rt, err := github.NewAppTransport(appID, installationID, keyPath)
	if err != nil {
		return err
	}

	opts := []github.Option{github.WithTimeout(timeout)}
	if endpoint != "" {
		opts = append(opts, github.WithEndpoint(endpoint))
	}
	client := github.New(rt, opts...)

	handler, err := tool.New(tool.Config{
		Client:         client,
		Transport:      rt,
		Roots:          roots,
		MaxCommitBytes: maxCommitBytes,
	})
	if err != nil {
		return err
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "push-verified", Version: version}, nil)
	handler.Register(srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}
