// Command church-mcp is the church's read-only MCP server: AI tools such as
// Claude and ChatGPT connect with OAuth, the person signs in through User
// Management, and the tools read the church's apps as that person.
//
//	church-mcp                                     start the web server
//	church-mcp connection set-sso URL CLIENT_ID [BASE_URL]
//	                                               first setup: save single sign-on (secret on stdin)
//	church-mcp connection clear ID                 break-glass: forget a connection's saved settings
//	church-mcp backup FILE                         write a consistent copy of the database
//
// Flags such as -db go before the command. Configuration comes from flags
// or environment variables (see README.md).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // lets TZ work in minimal containers with no zoneinfo

	"github.com/tylergoza/church-mcp/internal/server"
	"github.com/tylergoza/church-mcp/internal/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// churchZone is used when TZ isn't set: the church's apps all show local
// time without a zone, and that's Eastern.
const churchZone = "America/New_York"

func main() {
	addr := flag.String("addr", env("ADDR", ":8150"), "listen address (env ADDR)")
	dbPath := flag.String("db", env("DB_PATH", "data/mcp.db"), "SQLite database path (env DB_PATH)")
	dev := flag.Bool("dev", env("DEV", "") == "1", "serve templates/static from ./web and reload on each request (env DEV=1)")
	trustProxy := flag.Bool("trust-proxy", env("TRUST_PROXY", "") == "1", "trust X-Forwarded-* headers from a reverse proxy (env TRUST_PROXY=1)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	loc := time.Local
	if os.Getenv("TZ") == "" {
		if l, err := time.LoadLocation(churchZone); err == nil {
			loc = l
		}
	}

	args := flag.Args()
	if err := checkCommand(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("open database", "path", *dbPath, "err", err,
			"hint", fmt.Sprintf("make sure the folder exists and user id %d can write to it", os.Getuid()))
		os.Exit(1)
	}
	defer st.Close()

	if len(args) > 0 {
		if err := runCommand(st, args); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	srv, err := server.New(server.Config{Dev: *dev, TrustProxy: *trustProxy, Location: loc}, st, logger)
	if err != nil {
		logger.Error("init server", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Clear out expired sessions, codes and tokens.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := st.PurgeExpired(); err != nil {
				logger.Error("purge expired", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	go func() {
		logger.Info("listening", "addr", *addr, "db", *dbPath, "dev", *dev, "tz", loc.String())
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutdownCtx)
}

var commandUsage = map[string]string{
	"connection": "connection set-sso URL CLIENT_ID [BASE_URL]  |  connection clear " + strings.Join(server.ConnectionIDs(), "|"),
	"backup":     "backup DEST_FILE",
}

// checkCommand validates a command line before anything touches the
// database. Flags belong before the command; after it they'd be ignored.
func checkCommand(args []string) error {
	if len(args) == 0 {
		return nil
	}
	usage, ok := commandUsage[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q (commands: connection, backup)", args[0])
	}
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("flags go before the command: church-mcp -db PATH %s", usage)
		}
	}
	n := len(args) - 1
	switch {
	case args[0] == "backup" && n == 1:
		return nil
	case args[0] == "connection" && n == 2 && args[1] == "clear" && server.IsConnectionID(args[2]):
		return nil
	case args[0] == "connection" && (n == 3 || n == 4) && args[1] == "set-sso":
		return nil
	}
	return fmt.Errorf("usage: church-mcp %s", usage)
}

func runCommand(st *store.Store, args []string) error {
	switch args[0] {
	case "backup":
		if err := st.Backup(context.Background(), args[1]); err != nil {
			return err
		}
		fmt.Println("backup written to", args[1])
	case "connection":
		if args[1] == "clear" {
			if err := server.ClearConnection(st, args[2]); err != nil {
				return err
			}
			fmt.Printf("saved settings for %s removed\n", args[2])
			return nil
		}
		fmt.Fprint(os.Stderr, "Client secret (from User Management's Apps page): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no client secret on standard input")
		}
		fmt.Fprintln(os.Stderr)
		secret := strings.TrimSpace(line)
		if secret == "" {
			return errors.New("the client secret is empty")
		}
		base := ""
		if len(args) == 5 {
			base = args[4]
		}
		if err := server.SetSSO(st, args[2], args[3], secret, base); err != nil {
			return err
		}
		fmt.Println("single sign-on saved; restart the server if it's running")
	}
	return nil
}
