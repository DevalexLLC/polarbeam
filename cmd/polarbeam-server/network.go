package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/devalexllc/polarbeam/internal/server/networkadmin"
)

// cliNetworkFields makes shared validation errors name the CLI flags.
var cliNetworkFields = networkadmin.FieldNames{Name: "--name"}

func cmdNetwork(args []string) error {
	const use = "usage: polarbeam-server network list|create|set|delete --config <file> ..."
	if len(args) < 1 {
		return errors.New(use)
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("network list", flag.ExitOnError)
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		networks, err := st.ListNetworksConfig(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%-16s  %-20s  %6s  %6s  %6s  %6s\n", "NAME", "DISPLAY", "AGENTS", "TOKENS", "MESHES", "PROBES")
		for _, n := range networks {
			fmt.Printf("%-16s  %-20s  %6d  %6d  %6d  %6d\n",
				n.Name, n.DisplayName, n.AgentCount, n.TokenCount, n.MeshCount, n.ProbeCount)
		}
		return nil

	case "create":
		fs := flag.NewFlagSet("network create", flag.ExitOnError)
		name := fs.String("name", "", "network name (operator vocabulary, immutable once created)")
		displayName := fs.String("display-name", "", "human-friendly label shown in the dashboard")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		if problems := networkadmin.ValidateName(*name, cliNetworkFields); len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		id, err := st.CreateNetwork(ctx, *name, *displayName)
		if err != nil {
			return err
		}
		fmt.Printf("network %q created (%s)\n", *name, id)
		return nil

	case "set":
		fs := flag.NewFlagSet("network set", flag.ExitOnError)
		name := fs.String("name", "", "network to update")
		displayName := fs.String("display-name", "", "new display label (an explicit empty string clears it)")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		problems := networkadmin.ValidateName(*name, cliNetworkFields)
		// fs.Visit presence tracking, like site set: an explicit empty
		// --display-name is a real value (clear), an omitted flag is a
		// no-op request worth refusing loudly.
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["display-name"] {
			problems = append(problems, "nothing to set: pass --display-name")
		}
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		if err := st.UpdateNetwork(ctx, *name, *displayName); err != nil {
			return err
		}
		fmt.Printf("network %q updated\n", *name)
		return nil

	case "delete":
		fs := flag.NewFlagSet("network delete", flag.ExitOnError)
		name := fs.String("name", "", "network to delete (must be unreferenced; unused join tokens are swept)")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		if problems := networkadmin.ValidateName(*name, cliNetworkFields); len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()
		deleted, err := st.DeleteNetwork(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Printf("network %q deleted (%d unused join token(s) removed with it)\n", *name, deleted)
		return nil
	}
	return fmt.Errorf("unknown network subcommand %q\n%s", args[0], use)
}
