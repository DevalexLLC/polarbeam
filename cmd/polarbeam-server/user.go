package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/term"

	"github.com/devalexllc/polarbeam/internal/server/auth"
	"github.com/devalexllc/polarbeam/internal/server/store"
)

// stringList is a repeatable string flag (--network a --network b).
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func cmdUser(args []string) error {
	const use = "usage: polarbeam-server user add --config <file> --username <name> [--role <role>] [--network <name>]... [--admin]"
	if len(args) < 1 {
		return fmt.Errorf(use)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		username := fs.String("username", "", "login name for the new user")
		role := fs.String("role", "", "role: "+strings.Join(store.Roles, ", ")+" (default viewer)")
		admin := fs.Bool("admin", false, "shorthand for --role admin")
		var networks stringList
		fs.Var(&networks, "network", "network the user may see (repeatable; required for the network-scoped roles)")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		if *username == "" {
			return fmt.Errorf("--username is required")
		}
		if *admin && *role != "" && *role != store.RoleAdmin {
			return fmt.Errorf("--admin conflicts with --role %s", *role)
		}
		r := *role
		if r == "" {
			r = store.RoleViewer
			if *admin {
				r = store.RoleAdmin
			}
		}
		if !store.ValidRole(r) {
			return fmt.Errorf("--role must be one of: %s", strings.Join(store.Roles, ", "))
		}
		if store.RoleIsNetworkScoped(r) && len(networks) == 0 {
			return fmt.Errorf("--role %s requires at least one --network", r)
		}
		if !store.RoleIsNetworkScoped(r) && len(networks) > 0 {
			return fmt.Errorf("--network is only valid with the network-scoped roles")
		}
		fd := int(os.Stdin.Fd())
		pw, err := readNewPassword(os.Stdin, fd, term.IsTerminal(fd))
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		// Names resolve against existing networks only — a typo'd network
		// must fail loudly, never widen or shrink a tenant's scope.
		var networkIDs []uuid.UUID
		for _, name := range networks {
			id, err := st.NetworkIDByName(ctx, name)
			if err != nil {
				return err
			}
			networkIDs = append(networkIDs, id)
		}
		id, err := st.CreateUser(ctx, *username, hash, r, networkIDs)
		if err != nil {
			return err
		}
		if len(networks) > 0 {
			fmt.Printf("user %q created (role %s, networks %s, %s)\n", *username, r, strings.Join(networks, ","), id)
		} else {
			fmt.Printf("user %q created (role %s, %s)\n", *username, r, id)
		}
		return nil
	default:
		return fmt.Errorf("unknown user subcommand %q\n%s", args[0], use)
	}
}

// readNewPassword collects the new user's password. On a terminal it prompts
// twice without echo; otherwise it reads a single line from in (piped stdin:
// `printf 'secret' | polarbeam-server user add ...`).
func readNewPassword(in io.Reader, fd int, isTerminal bool) (string, error) {
	var pw string
	if isTerminal {
		fmt.Fprint(os.Stderr, "Password: ")
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		second, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password confirmation: %w", err)
		}
		if string(first) != string(second) {
			return "", fmt.Errorf("passwords do not match")
		}
		pw = string(first)
	} else {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		pw = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	}
	// Runes, not bytes — the policy says characters (matches the dashboard's
	// self-service check).
	if utf8.RuneCountInString(pw) < auth.MinPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", auth.MinPasswordLen)
	}
	return pw, nil
}
