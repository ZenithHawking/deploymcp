// Command deploymcp-agent is the DeployMCP VPS agent.
//
//	deploymcp-agent init   [--data-dir DIR]                 create DB + first admin token
//	deploymcp-agent serve  [--listen ADDR] [--domain D] ... run the HTTPS API
//	deploymcp-agent token  create|list|revoke ...           manage scoped tokens
//	deploymcp-agent version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ZenithHawking/deploymcp/agent/internal/api"
	"github.com/ZenithHawking/deploymcp/agent/internal/auth"
	"github.com/ZenithHawking/deploymcp/agent/internal/caddy"
	"github.com/ZenithHawking/deploymcp/agent/internal/deployer"
	"github.com/ZenithHawking/deploymcp/agent/internal/store"
)

// Version is set at build time via -ldflags "-X main.Version=v0.1.0".
var Version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprintf(os.Stderr, `deploymcp-agent %s — Zero-SSH Docker deploys for AI coding agents

Usage:
  deploymcp-agent init    [--data-dir DIR] [--force]
  deploymcp-agent serve   [--data-dir DIR] [--listen ADDR] [--domain DOMAIN]
                          [--caddy-admin URL|off] [--acme-email EMAIL] [--public-host HOST] [--tunnel] [--caddy-listen ADDR]
  deploymcp-agent token create --apps 'web,api'|'*' --perm read|write [--name NAME]
  deploymcp-agent token list
  deploymcp-agent token revoke ID
  deploymcp-agent version

Environment: DEPLOYMCP_DATA_DIR, DEPLOYMCP_LISTEN, DEPLOYMCP_DOMAIN,
             DEPLOYMCP_CADDY_ADMIN, DEPLOYMCP_ACME_EMAIL, DEPLOYMCP_PUBLIC_HOST, DEPLOYMCP_TUNNEL,
             DEPLOYMCP_CADDY_LISTEN
`, Version)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("deploymcp: ")
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "token":
		err = cmdToken(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", env("DEPLOYMCP_DATA_DIR", "/var/lib/deploymcp"), "data directory")
}

func openStore(dataDir string) (*store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "apps"), 0o755); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dataDir, "deploymcp.db"))
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dataDir := dataDirFlag(fs)
	force := fs.Bool("force", false, "create another admin token even if tokens exist")
	quiet := fs.Bool("token-only", false, "print only the token")
	fs.Parse(args)
	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	existing, err := st.ListTokens()
	if err != nil {
		return err
	}
	if len(existing) > 0 && !*force {
		if !*quiet {
			fmt.Printf("Already initialized at %s (%d token(s)). Use `token create` or --force.\n", *dataDir, len(existing))
		}
		return nil
	}
	tok, hash, err := auth.Generate()
	if err != nil {
		return err
	}
	if _, err := st.CreateToken("admin", hash, "*", auth.PermWrite); err != nil {
		return err
	}
	if *quiet {
		fmt.Println(tok)
		return nil
	}
	fmt.Printf("Initialized %s\nAdmin token (scope: * write) — shown once, store it safely:\n\n  %s\n\n", *dataDir, tok)
	return nil
}

func cmdToken(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: token create|list|revoke")
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ExitOnError)
	dataDir := dataDirFlag(fs)
	switch args[0] {
	case "create":
		apps := fs.String("apps", "", "comma-separated app names, or '*'")
		perm := fs.String("perm", "write", "read or write")
		name := fs.String("name", "", "label for this token")
		fs.Parse(args[1:])
		scope, err := auth.ParseScope(*apps, *perm)
		if err != nil {
			return err
		}
		st, err := openStore(*dataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		tok, hash, err := auth.Generate()
		if err != nil {
			return err
		}
		id, err := st.CreateToken(*name, hash, scope.AppsString(), scope.Perm)
		if err != nil {
			return err
		}
		fmt.Printf("Token #%d (%s) — shown once:\n\n  %s\n\n", id, scope, tok)
		return nil
	case "list":
		fs.Parse(args[1:])
		st, err := openStore(*dataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		toks, err := st.ListTokens()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tAPPS\tPERM\tCREATED\tLAST USED\tREVOKED")
		for _, t := range toks {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%v\n", t.ID, t.Name, t.Apps, t.Perm, t.CreatedAt, t.LastUsed, t.Revoked)
		}
		return tw.Flush()
	case "revoke":
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return errors.New("usage: token revoke ID")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid id %q", fs.Arg(0))
		}
		st, err := openStore(*dataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.RevokeToken(id); err != nil {
			return err
		}
		fmt.Printf("Token #%d revoked\n", id)
		return nil
	}
	return fmt.Errorf("unknown token command %q", args[0])
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := dataDirFlag(fs)
	listen := fs.String("listen", env("DEPLOYMCP_LISTEN", "127.0.0.1:7070"), "listen address for the API")
	domain := fs.String("domain", env("DEPLOYMCP_DOMAIN", ""), "public domain for the agent API (routed via Caddy)")
	caddyAdmin := fs.String("caddy-admin", env("DEPLOYMCP_CADDY_ADMIN", "http://localhost:2019"), "Caddy admin API URL, or 'off'")
	acmeEmail := fs.String("acme-email", env("DEPLOYMCP_ACME_EMAIL", ""), "email for Let's Encrypt")
	publicHost := fs.String("public-host", env("DEPLOYMCP_PUBLIC_HOST", ""), "public IP/host used in URLs of apps without a domain")
	tunnel := fs.Bool("tunnel", env("DEPLOYMCP_TUNNEL", "") == "1" || env("DEPLOYMCP_TUNNEL", "") == "true",
		"Cloudflare Tunnel mode: Caddy serves plain HTTP on :80, Cloudflare terminates TLS")
	caddyListen := fs.String("caddy-listen", env("DEPLOYMCP_CADDY_LISTEN", ":80"), "Caddy HTTP listen address in tunnel mode, e.g. 127.0.0.1:8088")
	fs.Parse(args)

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	cd := caddy.New(*caddyAdmin, *acmeEmail)
	cd.HTTPOnly = *tunnel
	cd.HTTPListen = *caddyListen
	dep := deployer.New(deployer.Config{
		DataDir:       *dataDir,
		PublicHost:    *publicHost,
		AgentDomain:   *domain,
		AgentUpstream: *listen,
	}, st, cd, deployer.CLIRunner{})

	if cd.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := dep.SyncCaddy(ctx); err != nil {
			log.Printf("warning: initial Caddy sync failed: %v", err)
		}
		cancel()
		if *tunnel {
			log.Printf("tunnel mode: Caddy serves HTTP on %s; point your Cloudflare Tunnel hostnames there", *caddyListen)
		}
	} else {
		log.Printf("Caddy integration disabled; apps are exposed on host ports without HTTPS")
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&api.Server{Store: st, Deployer: dep, Version: Version}).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      api.DeployTimeout + 2*time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Printf("agent %s listening on %s (data: %s)", Version, *listen, *dataDir)
		if *domain != "" {
			log.Printf("public API: https://%s", *domain)
		}
		errc <- srv.ListenAndServe()
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case s := <-sig:
		log.Printf("received %s, shutting down", s)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
