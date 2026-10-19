package cli

import (
	"changathi/internal/core"
	"changathi/internal/service"
	"changathi/internal/transcript"
	"fmt"
	"github.com/spf13/cobra"
	"strings"
	"text/tabwriter"
)

func (a *app) supervisor() *cobra.Command {
	cmd := &cobra.Command{Use: "supervisor", Short: "Coordinate dependent issues through human review gates"}
	var project string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		s, err := a.client().Status(c.Context())
		if err != nil {
			return err
		}
		out := []core.Supervisor{}
		for _, v := range s.Supervisors {
			for _, p := range s.Projects {
				if v.ProjectID == p.ID && (project == "" || project == p.Name) {
					out = append(out, v)
				}
			}
		}
		if a.json {
			return a.print(out)
		}
		if len(out) == 0 {
			_, err := fmt.Fprintln(a.out, "No supervisors found.")
			return err
		}
		names := map[string]string{}
		for _, p := range s.Projects {
			names[p.ID] = p.Name
		}
		clean := func(s string) string { return strings.Join(strings.Fields(transcript.Clean(s)), " ") }
		w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ISSUE\tSUBJECT\tSTATUS\tCOMPLETED\tCURRENT\tDETAIL")
		for _, v := range out {
			completed := 0
			for _, child := range v.Children {
				if child.State == "completed" {
					completed++
				}
			}
			current := "—"
			if v.Current != 0 {
				current = fmt.Sprintf("#%d", v.Current)
			}
			fmt.Fprintf(w, "%s#%d\t%s\t%s\t%d/%d\t%s\t%s\n", clean(names[v.ProjectID]), v.Parent.Number, clean(v.Parent.Title), strings.ReplaceAll(clean(v.State), "_", " "), completed, len(v.Plan.Children), current, clean(v.Reason))
		}
		return w.Flush()
	}}
	list.Flags().StringVar(&project, "project", "", "Filter by registered project")
	cmd.AddCommand(list)
	for _, action := range []string{"validate", "start", "show", "pause", "resume", "cancel", "reconcile"} {
		action := action
		var ack bool
		var child, pr int
		var evidence string
		c := &cobra.Command{Use: action + " <project>#<parent>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
			p, n, err := reference(args[0])
			if err != nil {
				return err
			}
			return a.action(c.Context(), service.Request{Action: "supervisor." + action, Project: p, Number: n, Child: child, PR: pr, AckCompletion: ack, Evidence: evidence})
		}}
		if action == "reconcile" {
			c.Flags().IntVar(&child, "child", 0, "Child issue number")
			_ = c.MarkFlagRequired("child")
			c.Flags().IntVar(&pr, "pr", 0, "Associate a previously merged external PR")
			c.Flags().BoolVar(&ack, "ack-completion", false, "Attest a closure GitHub cannot attribute to you (bot or automatic)")
			c.Flags().StringVar(&evidence, "evidence", "", "HTTPS URL of operational evidence the delivery PR lacks")
		}
		cmd.AddCommand(c)
	}
	return cmd
}
