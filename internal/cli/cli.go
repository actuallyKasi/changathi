package cli

import (
	"changathi/internal/core"
	"changathi/internal/provider"
	"changathi/internal/service"
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

type app struct {
	config     core.Config
	configFile string
	json       bool
	out        io.Writer
}

func New() *cobra.Command {
	a := &app{config: core.DefaultConfig(), out: os.Stdout}
	root := &cobra.Command{Use: "changathi", Short: "Run coding agents across your GitHub repositories", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&a.config.DataDir, "data-dir", a.config.DataDir, "Daemon state and workspace directory")
	root.PersistentFlags().StringVar(&a.configFile, "config", "", "Service YAML configuration")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "Machine-readable JSON output")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		a.out = cmd.OutOrStdout()
		if a.configFile != "" {
			b, err := os.ReadFile(a.configFile)
			if err != nil {
				return err
			}
			oldDir := a.config.DataDir
			oldConcurrency, oldPoll, oldCodex, oldClaude := a.config.Concurrency, a.config.PollSeconds, a.config.Codex, a.config.Claude
			decoder := yaml.NewDecoder(strings.NewReader(string(b)))
			decoder.KnownFields(true)
			if err = decoder.Decode(&a.config); err != nil {
				return err
			}
			if cmd.Flags().Changed("data-dir") {
				a.config.DataDir = oldDir
			}
			if cmd.Flags().Changed("concurrency") {
				a.config.Concurrency = oldConcurrency
			}
			if cmd.Flags().Changed("poll-seconds") {
				a.config.PollSeconds = oldPoll
			}
			if cmd.Flags().Changed("codex") {
				a.config.Codex = oldCodex
			}
			if cmd.Flags().Changed("claude") {
				a.config.Claude = oldClaude
			}
		}
		dir, err := filepath.Abs(a.config.DataDir)
		if err != nil {
			return err
		}
		a.config.DataDir = dir
		return a.config.Validate()
	}
	root.AddCommand(a.daemon(), a.summary(), a.project(), a.issue(), a.supervisor(), a.agent(), a.workspace(), a.skill(), &cobra.Command{Use: "usage", Short: "Read reported provider account limits", RunE: func(c *cobra.Command, _ []string) error {
		return a.action(c.Context(), service.Request{Action: "usage"})
	}}, a.doctorCommand())
	return root
}
func (a *app) client() *Client { return NewClient(a.config.Socket()) }
func (a *app) print(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func (a *app) action(ctx context.Context, r service.Request) error {
	var v any
	if err := a.client().Action(ctx, r, &v); err != nil {
		return err
	}
	return a.display(v)
}
func (a *app) daemon() *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Short: "Manage the daemon"}
	start := &cobra.Command{Use: "start", Short: "Start the daemon in the foreground", RunE: func(c *cobra.Command, _ []string) error { return service.Serve(c.Context(), a.config) }}
	start.Flags().IntVar(&a.config.Concurrency, "concurrency", 1, "Global agent limit")
	start.Flags().IntVar(&a.config.PollSeconds, "poll-seconds", 30, "GitHub poll interval")
	start.Flags().StringVar(&a.config.Codex, "codex", "codex", "Codex executable")
	start.Flags().StringVar(&a.config.Claude, "claude", "claude", "Claude Code executable")
	cmd.AddCommand(start, a.daemonInstall(), a.daemonUninstall(), &cobra.Command{Use: "status", RunE: func(c *cobra.Command, _ []string) error {
		s, e := a.client().Status(c.Context())
		if e != nil {
			return e
		}
		return a.status(s)
	}}, &cobra.Command{Use: "stop", RunE: func(c *cobra.Command, _ []string) error {
		return a.action(c.Context(), service.Request{Action: "daemon.stop"})
	}})
	return cmd
}
func (a *app) project() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Register and manage repositories"}
	var repo string
	var auto bool
	var concurrency int
	add := &cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.action(c.Context(), service.Request{Action: "project.add", Project: args[0], Repo: repo, Auto: auto, Concurrency: concurrency})
	}}
	add.Flags().StringVar(&repo, "repo", "", "GitHub owner/repo")
	_ = add.MarkFlagRequired("repo")
	add.Flags().BoolVar(&auto, "auto", false, "Enable automatic agent:ready pickup")
	add.Flags().IntVar(&concurrency, "concurrency", 1, "Project agent limit")
	cmd.AddCommand(add, &cobra.Command{Use: "list", RunE: func(c *cobra.Command, _ []string) error {
		s, e := a.client().Status(c.Context())
		if e != nil {
			return e
		}
		if a.json {
			return a.print(s.Projects)
		}
		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tREPOSITORY\tENABLED\tAUTO\tLIMIT")
		for _, p := range s.Projects {
			fmt.Fprintf(w, "%s\t%s\t%t\t%t\t%d\n", p.Name, p.Repo, p.Enabled, p.Auto, p.Concurrency)
		}
		return w.Flush()
	}})
	for _, action := range []string{"pause", "resume"} {
		action := action
		cmd.AddCommand(&cobra.Command{Use: action + " <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
			return a.action(c.Context(), service.Request{Action: "project." + action, Project: args[0]})
		}})
	}
	return cmd
}
func reference(ref string) (string, int, error) {
	parts := strings.Split(ref, "#")
	if len(parts) != 2 {
		return "", 0, fmt.Errorf("issue reference must be project#number")
	}
	n, e := strconv.Atoi(parts[1])
	if e != nil || n < 1 {
		return "", 0, fmt.Errorf("invalid issue number")
	}
	return parts[0], n, nil
}
func (a *app) issue() *cobra.Command {
	cmd := &cobra.Command{Use: "issue", Short: "List and dispatch GitHub issues (create and edit issues with gh)"}
	var project string
	var all bool
	list := &cobra.Command{Use: "list", RunE: func(c *cobra.Command, _ []string) error {
		if project == "" && !all {
			return fmt.Errorf("specify --project or --all")
		}
		s, e := a.client().Status(c.Context())
		if e != nil {
			return e
		}
		out := []map[string]any{}
		for _, p := range s.Projects {
			if !all && p.Name != project {
				continue
			}
			var issues []core.Issue
			if e = a.client().Action(c.Context(), service.Request{Action: "issue.list", Project: p.Name}, &issues); e != nil {
				return e
			}
			for _, i := range issues {
				out = append(out, map[string]any{"project": p.Name, "issue": i})
			}
		}
		if a.json {
			return a.print(out)
		}
		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ISSUE\tTITLE\tREADY")
		for _, v := range out {
			i := v["issue"].(core.Issue)
			fmt.Fprintf(w, "%s#%d\t%s\t%t\n", v["project"], i.Number, i.Title, i.Ready())
		}
		return w.Flush()
	}}
	list.Flags().StringVar(&project, "project", "", "Project name")
	list.Flags().BoolVar(&all, "all", false, "All registered projects")
	cmd.AddCommand(list)
	for _, action := range []string{"show", "run", "retry", "cancel"} {
		action := action
		cmd.AddCommand(&cobra.Command{Use: action + " <project>#<number>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
			p, n, e := reference(args[0])
			if e != nil {
				return e
			}
			return a.action(c.Context(), service.Request{Action: "issue." + action, Project: p, Number: n})
		}})
	}
	return cmd
}
func (a *app) agent() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Inspect and stop agents"}
	var filter agentFilter
	list := &cobra.Command{Use: "list", Short: "Recent agents, newest first", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, e := a.client().Status(c.Context())
		if e != nil {
			return e
		}
		return a.agentRows(agentRows(s, filter))
	}}
	list.Flags().BoolVar(&filter.active, "active", false, "Only agents that are still working")
	list.Flags().StringVar(&filter.project, "project", "", "Filter by registered project")
	list.Flags().IntVar(&filter.limit, "limit", 20, "Maximum agents to show (0 for all)")
	cmd.AddCommand(list)
	var full bool
	show := &cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		s, e := a.client().Status(c.Context())
		if e != nil {
			return e
		}
		for _, v := range s.Agents {
			if v.ID == args[0] {
				return a.display(agentDetail(v, full))
			}
		}
		return fmt.Errorf("unknown agent")
	}}
	show.Flags().BoolVar(&full, "full", false, "Include the recorded workflow and prompt")
	cmd.AddCommand(show, &cobra.Command{Use: "stop <id>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.action(c.Context(), service.Request{Action: "agent.stop", Attempt: args[0]})
	}})
	var o logOptions
	l := &cobra.Command{Use: "logs <id>", Short: "Agent transcript as plain text", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		o.raw = o.raw || a.json
		return a.readableLogs(c.Context(), args[0], o)
	}}
	l.Flags().BoolVar(&o.follow, "follow", false, "Follow agent events")
	l.Flags().BoolVar(&o.raw, "raw", false, "Show individual provider events")
	l.Flags().IntVar(&o.tail, "tail", 0, "Show only the last N transcript items")
	l.Flags().BoolVar(&o.last, "last", false, "Start from the agent's last message")
	l.Flags().BoolVar(&o.full, "full", false, "Include output of successful tools and diffs")
	cmd.AddCommand(l)
	return cmd
}
func (a *app) workspace() *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Short: "Clean delivered worktrees"}
	var apply bool
	clean := &cobra.Command{Use: "clean <agent-id>", Short: "Preview cleanup; --apply removes a clean delivered worktree", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		return a.action(c.Context(), service.Request{Action: "workspace.clean", Attempt: args[0], Apply: apply})
	}}
	clean.Flags().BoolVar(&apply, "apply", false, "Remove the worktree")
	cmd.AddCommand(clean)
	return cmd
}
func (a *app) status(s core.Snapshot) error {
	if a.json {
		return a.print(s)
	}
	fmt.Fprintf(a.out, "Changathi: %d projects, %d recorded agents\n", len(s.Projects), len(s.Agents))
	a.printGates(s.ProviderWaits)
	if s.Usage != nil {
		fmt.Fprintln(a.out, "Provider limits:")
		if err := a.display(s.Usage); err != nil {
			return err
		}
	}
	if len(s.Supervisors) > 0 {
		names := map[string]string{}
		for _, p := range s.Projects {
			names[p.ID] = p.Name
		}
		for _, v := range s.Supervisors {
			fmt.Fprintf(a.out, "SUPERVISOR %s#%d: %s — %s\n", names[v.ProjectID], v.Parent.Number, v.State, v.Reason)
		}
	}
	return a.agentRows(agentRows(s, agentFilter{limit: 20}))
}
func (a *app) doctorCommand() *cobra.Command {
	var models bool
	cmd := &cobra.Command{Use: "doctor", Short: "Check local dependencies and authentication", RunE: func(c *cobra.Command, _ []string) error { return a.doctor(c.Context(), models) }}
	cmd.Flags().BoolVar(&models, "models", false, "Run one minimal turn per allowed Claude model (uses a little quota)")
	return cmd
}

// enabledProviders lists the providers the host configuration permits.
func (a *app) enabledProviders() []string {
	if a.config.Providers == nil {
		return []string{"codex"}
	}
	out := []string{}
	for _, name := range core.Providers {
		if _, ok := a.config.Providers[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

func (a *app) doctor(ctx context.Context, models bool) error {
	results := map[string]string{}
	failed := false
	check := func(name string, args ...string) (string, bool) {
		out, e := core.Command(ctx, "", args[0], args[1:]...)
		if e != nil {
			failed = true
			results[name] = e.Error()
			return "", false
		}
		results[name] = out
		return out, true
	}
	tools := []string{"git", "gh"}
	executables := map[string]string{"codex": a.config.Codex, "claude": a.config.Claude}
	enabled := a.enabledProviders()
	for _, name := range enabled {
		tools = append(tools, executables[name])
	}
	for _, tool := range tools {
		p, e := exec.LookPath(tool)
		if e != nil {
			results[tool] = e.Error()
			failed = true
		} else {
			results[tool] = p
		}
	}
	check("github_auth", "gh", "auth", "status")
	for _, name := range enabled {
		switch name {
		case "codex":
			check("codex_auth", a.config.Codex, "login", "status")
		case "claude":
			if version, ok := check("claude_version", a.config.Claude, "--version"); ok && !strings.HasPrefix(version, provider.ClaudeTestedVersion+" ") {
				results["claude_version"] = fmt.Sprintf("%s (warning: tested with %s)", version, provider.ClaudeTestedVersion)
			}
			if out, ok := check("claude_auth", a.config.Claude, "auth", "status"); ok {
				var status struct {
					LoggedIn   bool   `json:"loggedIn"`
					AuthMethod string `json:"authMethod"`
				}
				if json.Unmarshal([]byte(out), &status) == nil {
					if status.LoggedIn {
						results["claude_auth"] = "logged in (" + status.AuthMethod + ")"
					} else {
						failed = true
						results["claude_auth"] = "not logged in; run `claude auth login`"
					}
				}
			}
			allowed := a.config.Providers["claude"].Models
			results["claude_models"] = strings.Join(allowed, ", ")
			if len(allowed) == 0 {
				results["claude_models"] = "any (no allow-list)"
			}
			if models {
				dir, err := os.MkdirTemp("", "changathi-doctor-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
				for _, m := range allowed {
					probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					status, err := provider.Claude{Command: a.config.Claude}.Probe(probeCtx, dir, m)
					cancel()
					if err != nil {
						failed = true
						status = err.Error()
					}
					results["model "+m] = status
				}
			}
		}
	}
	if err := a.display(results); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("one or more checks failed")
	}
	return nil
}

func (a *app) printGates(gates []core.ProviderGate) {
	for _, g := range gates {
		fmt.Fprintf(a.out, "Provider %s paused until %s: %s\n", core.GateKey(g.Provider, g.Model), g.Until.Local().Format(time.RFC3339), singleLine(g.Reason))
	}
}
