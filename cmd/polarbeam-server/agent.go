package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"strings"

	"github.com/google/uuid"

	"github.com/devalexllc/polarbeam/internal/audit"
)

func cmdAgent(args []string) error {
	const use = "usage: polarbeam-server agent revoke --config <file> (--agent <uuid> | --serial <n>)"
	if len(args) < 1 {
		return errors.New(use)
	}
	switch args[0] {
	case "revoke":
		fs := flag.NewFlagSet("agent revoke", flag.ExitOnError)
		agentFlag := fs.String("agent", "", "revoke every unrevoked certificate of this agent (UUID)")
		serialFlag := fs.String("serial", "", "revoke exactly one certificate by decimal serial")
		cfg, err := loadConfig(fs, args[1:])
		if err != nil {
			return err
		}
		agentID, serial, problems := agentRevokeArgs(*agentFlag, *serialFlag)
		if len(problems) > 0 {
			return errors.New(strings.Join(problems, "; "))
		}
		st, ctx, cancel, err := adminStore(cfg)
		if err != nil {
			return err
		}
		defer cancel()
		defer st.Close()

		if serial == nil {
			serials, err := st.RevokeAgentCertificates(ctx, agentID)
			if err != nil {
				return err
			}
			auditCLI(audit.EventCLIAgentRevoke, "agent certificates revoked", audit.Success,
				slog.String("agent", agentID.String()), slog.String("serials", strings.Join(serials, ",")))
			if len(serials) == 0 {
				fmt.Printf("agent %s has no unrevoked certificates; nothing to do\n", agentID)
			}
			for _, s := range serials {
				fmt.Printf("revoked certificate %s of agent %s\n", s, agentID)
			}
		} else {
			owner, live, err := st.RevokeCertificate(ctx, serial)
			if err != nil {
				return err
			}
			auditCLI(audit.EventCLIAgentRevoke, "agent certificate revoked", audit.Success,
				slog.String("agent", owner.String()), slog.String("serial", serial.String()))
			fmt.Printf("revoked certificate %s of agent %s\n", serial, owner)
			if live > 0 {
				fmt.Printf("WARNING: %d other certificate(s) of agent %s remain valid; "+
					"revoke the agent with --agent %s if it is compromised\n", live, owner, owner)
			}
		}
		fmt.Println("live agent streams using a revoked certificate drop within about 30 seconds; " +
			"the agent's join token can no longer be replayed to re-enroll it")
		return nil
	default:
		return errors.New(use)
	}
}

// agentRevokeArgs validates the revoke target, naming every problem at
// once. Exactly one of --agent and --serial is required; a nil serial
// means the agent-wide form.
func agentRevokeArgs(agent, serial string) (uuid.UUID, *big.Int, []string) {
	var problems []string
	switch {
	case agent == "" && serial == "":
		return uuid.Nil, nil, []string{"one of --agent or --serial is required"}
	case agent != "" && serial != "":
		return uuid.Nil, nil, []string{"--agent and --serial are mutually exclusive"}
	}
	var id uuid.UUID
	if agent != "" {
		var err error
		if id, err = uuid.Parse(agent); err != nil {
			problems = append(problems, fmt.Sprintf("--agent %q is not a UUID", agent))
		}
		return id, nil, problems
	}
	n, ok := new(big.Int).SetString(serial, 10)
	if !ok || n.Sign() < 0 {
		return uuid.Nil, nil, []string{fmt.Sprintf("--serial %q is not a non-negative decimal integer", serial)}
	}
	return uuid.Nil, n, nil
}
