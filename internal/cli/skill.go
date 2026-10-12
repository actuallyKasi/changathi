package cli

import (
	"changathi/skills"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func (a *app) skill() *cobra.Command {
	cmd := &cobra.Command{Use: "skill", Short: "Install the Claude skill for driving Changathi"}
	var dir string
	var print bool
	install := &cobra.Command{Use: "install", Short: "Write SKILL.md to ~/.claude/skills/changathi (or --dir <repo>/.claude/skills/changathi)", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if print {
			_, err := fmt.Fprint(a.out, skills.Changathi)
			return err
		}
		root := dir
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			root = home
		}
		target := filepath.Join(root, ".claude", "skills", "changathi", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(skills.Changathi), 0644); err != nil {
			return err
		}
		return a.display(map[string]string{"installed": target})
	}}
	install.Flags().StringVar(&dir, "dir", "", "Install into this repository instead of your home directory")
	install.Flags().BoolVar(&print, "print", false, "Print the skill instead of installing it")
	cmd.AddCommand(install)
	return cmd
}
