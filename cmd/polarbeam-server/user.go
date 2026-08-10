package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/devalexllc/polarbeam/internal/server/auth"
)

const minPasswordLen = 8

func cmdUser(args []string) error {
	const use = "usage: polarbeam-server user add --config <file> --username <name> [--admin]"
	if len(args) < 1 {
		return fmt.Errorf(use)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		username := fs.String("username", "", "login name for the new user")
		admin := fs.Bool("admin", false, "grant the admin role (default viewer)")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		if *username == "" {
			return fmt.Errorf("--username is required")
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
		role := "viewer"
		if *admin {
			role = "admin"
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		id, err := st.CreateUser(ctx, *username, hash, role)
		if err != nil {
			return err
		}
		fmt.Printf("user %q created (role %s, %s)\n", *username, role, id)
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
	if len(pw) < minPasswordLen {
		return "", fmt.Errorf("password must be at least %d characters", minPasswordLen)
	}
	return pw, nil
}
